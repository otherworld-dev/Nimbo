//go:build windows

// Package cfapi is an experimental integration with the Windows Cloud Files API
// (cldapi.dll) that powers on-demand ("online-only") files. This first layer
// registers and unregisters a folder as a cloud sync root — the foundation the
// placeholder + hydration layers build on. It is opt-in and non-destructive:
// registering a sync root does not alter the files inside it.
package cfapi

import (
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	cldapi                               = windows.NewLazySystemDLL("cldapi.dll")
	procCfRegisterSyncRoot               = cldapi.NewProc("CfRegisterSyncRoot")
	procCfUnregisterSyncRoot             = cldapi.NewProc("CfUnregisterSyncRoot")
	procCfConnectSyncRoot                = cldapi.NewProc("CfConnectSyncRoot")
	procCfDisconnectSyncRoot             = cldapi.NewProc("CfDisconnectSyncRoot")
	procCfCreatePlaceholders             = cldapi.NewProc("CfCreatePlaceholders")
	procCfExecute                        = cldapi.NewProc("CfExecute")
	procCfConvertToPlaceholder           = cldapi.NewProc("CfConvertToPlaceholder")
	procCfSetInSyncState                 = cldapi.NewProc("CfSetInSyncState")
	procCfGetPlaceholderStateFromAttrTag = cldapi.NewProc("CfGetPlaceholderStateFromAttributeTag")
	procCfUpdatePlaceholder              = cldapi.NewProc("CfUpdatePlaceholder")
	procCfSetPinState                    = cldapi.NewProc("CfSetPinState")
	procCfDehydratePlaceholder           = cldapi.NewProc("CfDehydratePlaceholder")
	procCfRevertPlaceholder              = cldapi.NewProc("CfRevertPlaceholder")
)

var procSHChangeNotify = windows.NewLazySystemDLL("shell32.dll").NewProc("SHChangeNotify")

var procRtlSetPlaceholderMode = windows.NewLazySystemDLL("ntdll.dll").NewProc("RtlSetProcessPlaceholderCompatibilityMode")

// ExposePlaceholders opts this process out of placeholder disguising
// (PHCM_EXPOSE_PLACEHOLDERS) so attribute probes report real cloud state. Call
// once at app startup. See the disguising GOTCHA above for scope and safety.
func ExposePlaceholders() {
	if err := procRtlSetPlaceholderMode.Find(); err == nil {
		_, _, _ = procRtlSetPlaceholderMode.Call(2)
	}
}

// ShellNotifyUpdated tells Explorer one item's state changed so it redraws the
// row (glyph included) without a manual refresh. State surgery below the shell
// (CfConvertToPlaceholder / CfSetInSyncState) generates no shell notification
// of its own — heals looked like they did nothing until F5 or an Explorer
// restart. Fire-and-forget; per-item cost is fine at heal scale (dozens), keep
// it away from adopt-scale loops (hundreds of thousands).
func ShellNotifyUpdated(path string) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return
	}
	const (
		shcneUpdateItem = 0x00002000
		shcnfPathW      = 0x0005
	)
	_, _, _ = procSHChangeNotify.Call(shcneUpdateItem, shcnfPathW, uintptr(unsafe.Pointer(p)), 0)
}

// GOTCHA — placeholder DISGUISING (the #580 false alarm and the live-mode
// 121k-failure bug): cfapi hides reparse points from every process that is not
// a connected sync engine for the root or a %systemroot% binary — a MANIFEST
// alone does NOT exempt an app. A disguised HYDRATED placeholder reads as a
// PLAIN file (attrs 0x20, state 0) while a dehydrated one keeps
// RECALL_ON_DATA_ACCESS. This faked "hydrated placeholders flatten on
// Disconnect" (fsutil, exempt, showed every reparse point intact), and made
// live-mode status walks re-convert already-converted files forever
// (0x8007017C per file). Remedies, all in use:
//   - ExposePlaceholders() below is called at app startup — safe on status
//     roots (measured: exposed probe 0x420, no error) and on connected
//     on-demand roots; the one measured hazard is probing a DISCONNECTED
//     on-demand root (ERROR_CLOUD_FILE_PROVIDER_NOT_RUNNING), which
//     production never does (probes only run while mounted).
//   - MarkInSync treats 0x8007017C as "already a placeholder" and asserts
//     in-sync instead of failing.
//   - Unmanifested TEST binaries stay disguised unless they opt in; tests
//     probing across a Disconnect use fsutil (exempt) as the observer.

// providerID identifies Nimbo as the sync provider (fixed GUID).
// {7C9F2B41-5E3A-4D88-9C16-2A0F8B7D3E20}
var providerID = windows.GUID{
	Data1: 0x7c9f2b41, Data2: 0x5e3a, Data3: 0x4d88,
	Data4: [8]byte{0x9c, 0x16, 0x2a, 0x0f, 0x8b, 0x7d, 0x3e, 0x20},
}

// CF_HYDRATION_POLICY / CF_POPULATION_POLICY are USHORT primary+modifier pairs.
type hydrationPolicy struct{ Primary, Modifier uint16 }
type populationPolicy struct{ Primary, Modifier uint16 }

// CF_SYNC_POLICIES (Win10 1709 base layout).
type syncPolicies struct {
	StructSize                  uint32
	Hydration                   hydrationPolicy
	Population                  populationPolicy
	InSyncPolicy                uint32
	HardLinkPolicy              uint32
	PlaceholderManagementPolicy uint32
}

// CF_SYNC_REGISTRATION. Explicit padding keeps the 8-byte pointer alignment that
// the C struct has on amd64.
type syncRegistration struct {
	StructSize             uint32
	_                      uint32
	ProviderName           *uint16
	ProviderVersion        *uint16
	SyncRootIdentity       uintptr
	SyncRootIdentityLength uint32
	_                      uint32
	FileIdentity           uintptr
	FileIdentityLength     uint32
	// NO pad here: GUID is 4-aligned, so ProviderId starts at offset 52
	// directly after FileIdentityLength (cfapi.h; pinned by
	// TestStructLayoutsMatchCfapiH). A spurious pad shifted the GUID 4 bytes
	// and registered a mangled provider id for every root until 2026-08-18.
	ProviderID windows.GUID
}

const (
	cfHydrationPolicyFull                         = 2 // CF_HYDRATION_POLICY_PRIMARY_FULL
	cfHydrationPolicyAlwaysFull                   = 3 // CF_HYDRATION_POLICY_PRIMARY_ALWAYS_FULL
	cfPopulationPolicyPartial                     = 0 // CF_POPULATION_POLICY_PRIMARY_PARTIAL (on-demand dirs)
	cfPopulationPolicyFull                        = 2 // CF_POPULATION_POLICY_PRIMARY_FULL (we supply everything)
	cfRegisterFlagUpdate                          = 0x00000001
	cfRegisterFlagDisableOnDemandPopulationOnRoot = 0x00000002
)

// RegisterSyncRoot registers path as a Nimbo cloud sync root. Idempotent (uses
// the UPDATE flag). The identity is an opaque per-root blob (we use the path).
func RegisterSyncRoot(path string) error {
	// DisableOnDemandPopulationOnRoot: we seed the root's top level eagerly, so
	// the filter must NOT ask us to populate the root (that request would time
	// out — exactly the failure seen before). Subdirectories still populate
	// on-demand via FETCH_PLACEHOLDERS.
	return registerRoot(path, cfHydrationPolicyFull, cfPopulationPolicyPartial)
}

// RegisterStatusRoot registers path as a cloud sync root for STATUS ONLY: the
// files stay real on disk and are never dehydrated, but Windows draws its
// native sync-state icons on them in Explorer.
//
// This is how a packaged build can show overlays at all. A classic
// IShellIconOverlayIdentifier needs a CLSID under HKLM\...\ShellIconOverlayIdentifiers,
// and an MSIX app gets a virtualised registry with no HKLM write access, so that
// registration silently does nothing — which is why NCOverlays.dll has never
// worked for an installed build. The Cloud Files route gets to HKLM legitimately,
// by asking the system to write it (see RegisterShellSyncRoot), which is what
// OneDrive does.
//
// ALWAYS_FULL is the key: the platform then REFUSES any operation that would
// leave a file partly downloaded — CfDehydratePlaceholder and friends all fail —
// so a live folder cannot be turned into online-only files by accident.
func RegisterStatusRoot(path string) error {
	return registerRoot(path, cfHydrationPolicyAlwaysFull, cfPopulationPolicyFull)
}

// testRootRegisterFlags overrides the CfRegisterSyncRoot flags when non-zero.
// Set ONLY by tests probing the root-population storm (#569).
var testRootRegisterFlags uint32

// testRootPolicyOverride, when non-nil, overrides the CF_SYNC_POLICIES fields
// beyond the primary hydration/population values. Set ONLY by the #580
// flatten-on-disconnect experiments (OneDrive registers HydrationModifier=0x9,
// InSyncPolicy=0x111, HardLinkPolicy=1 — we register zeroes; the experiments
// isolate which delta keeps hydrated placeholders alive across a disconnect).
var testRootPolicyOverride *struct {
	HydrationModifier uint16
	PopulationPrimary uint16 // 0 = keep the caller's value
	InSyncPolicy      uint32
	HardLinkPolicy    uint32
}

func registerRoot(path string, hydration, population uint16) error {
	pathW, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	name, _ := windows.UTF16PtrFromString("Nimbo")
	ver, _ := windows.UTF16PtrFromString("1.0")
	idW, err := windows.UTF16FromString(path) // identity blob = the root path
	if err != nil {
		return err
	}

	reg := syncRegistration{
		ProviderName:           name,
		ProviderVersion:        ver,
		SyncRootIdentity:       uintptr(unsafe.Pointer(&idW[0])),
		SyncRootIdentityLength: uint32(len(idW) * 2),
		ProviderID:             providerID,
	}
	reg.StructSize = uint32(unsafe.Sizeof(reg))

	pol := syncPolicies{
		Hydration:  hydrationPolicy{Primary: hydration},
		Population: populationPolicy{Primary: population},
	}
	if o := testRootPolicyOverride; o != nil {
		pol.Hydration.Modifier = o.HydrationModifier
		if o.PopulationPrimary != 0 {
			pol.Population.Primary = o.PopulationPrimary
		}
		pol.InSyncPolicy = o.InSyncPolicy
		pol.HardLinkPolicy = o.HardLinkPolicy
	}
	pol.StructSize = uint32(unsafe.Sizeof(pol))

	regFlags := uint32(cfRegisterFlagUpdate | cfRegisterFlagDisableOnDemandPopulationOnRoot)
	if testRootRegisterFlags != 0 {
		regFlags = testRootRegisterFlags
	}
	hr, _, _ := procCfRegisterSyncRoot.Call(
		uintptr(unsafe.Pointer(pathW)),
		uintptr(unsafe.Pointer(&reg)),
		uintptr(unsafe.Pointer(&pol)),
		uintptr(regFlags),
	)
	runtime.KeepAlive(idW)
	runtime.KeepAlive(reg)
	if int32(hr) < 0 {
		return fmt.Errorf("CfRegisterSyncRoot: 0x%08x", uint32(hr))
	}
	return nil
}

// --- Shell-integration registration (SyncRootManager) ---
//
// CfRegisterSyncRoot only registers with the cloud-filter driver; it does NOT
// write the SyncRootManager metadata that Explorer needs to render a cloud
// folder (display name, icon, policies). Without it, Explorer crashes rendering
// the folder.
//
// That metadata lives in HKLM, which a packaged app cannot write. Replicating
// the keys by hand therefore never worked — it produced an HKCU entry Windows
// does not read, so no build has ever shown status icons. The registration now
// goes through the brokered WinRT API in syncroot_winrt_windows.go, which has
// the system write HKLM on our behalf. The two registrations are independent and
// may be done in either order (verified in syncroot_order_windows_test.go).

const syncRootManager = `Software\Microsoft\Windows\CurrentVersion\Explorer\SyncRootManager`

// shellRootID builds the SyncRootManager id "<provider>!<user SID>!<account>".
// The account segment is a hash of the local path so distinct sync roots get
// distinct registrations (a fixed segment made multiple mounts collide).
func shellRootID(path string) (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(strings.ToLower(path)))
	return fmt.Sprintf("Nimbo!%s!%08x", u.Uid, h.Sum32()), nil
}

// RegisterShellSyncRoot registers path with Explorer so it renders as a cloud
// folder and draws sync-state icons. displayName/iconPath are shown in the UI.
//
// This goes through the brokered WinRT API (see syncroot_winrt_windows.go),
// which is the only route that reaches HKLM — the hive Explorer reads. The
// direct registry writes below are a fallback for the case where the brokered
// call is unavailable; they land in HKCU, which Explorer ignores, so they buy
// nothing except leaving the old behaviour untouched rather than regressing to
// no registration at all.
func RegisterShellSyncRoot(path, displayName, iconPath string, pol ShellPolicy) error {
	id, err := shellRootID(path)
	if err != nil {
		return err
	}
	if err := registerShellSyncRootWinRT(id, path, displayName, iconPath+",0", "1.0", pol); err == nil {
		// Earlier versions wrote an HKCU entry with the same id. It never did
		// anything, but leaving it behind means two registrations claiming one
		// folder, so clear it once the real one is in place.
		removeLegacyHKCUEntry(id)
		return nil
	} else {
		slog.Warn("shell sync-root registration via WinRT failed; falling back to the registry",
			"path", path, "err", err)
	}
	return registerShellSyncRootRegistry(id, path, displayName, iconPath, pol)
}

func registerShellSyncRootRegistry(id, path, displayName, iconPath string, pol ShellPolicy) error {
	base := syncRootManager + `\` + id
	k, _, err := registry.CreateKey(registry.CURRENT_USER, base, registry.WRITE)
	if err != nil {
		return err
	}
	defer k.Close()
	_ = k.SetDWordValue("Flags", 0)
	_ = k.SetStringValue("DisplayNameResource", displayName)
	_ = k.SetStringValue("IconResource", iconPath+",0")
	_ = k.SetStringValue("Version", "1.0")
	_ = k.SetDWordValue("HydrationPolicy", pol.Hydration)
	_ = k.SetDWordValue("HydrationPolicyModifier", 0)
	_ = k.SetDWordValue("PopulationPolicy", pol.Population)
	_ = k.SetDWordValue("InSyncPolicy", pol.InSync)
	_ = k.SetDWordValue("HardlinkPolicy", 0)

	u, err := user.Current()
	if err != nil {
		return err
	}
	uk, _, err := registry.CreateKey(registry.CURRENT_USER, base+`\UserSyncRoots`, registry.WRITE)
	if err != nil {
		return err
	}
	defer uk.Close()
	return uk.SetStringValue(u.Uid, path)
}

// ShellSyncRootRegistered reports whether path is registered with Explorer as a
// cloud sync root. Such a folder already appears in the navigation pane under
// its provider name, so anything else that adds a nav-pane entry for the same
// folder — the delegate-folder registration in internal/shellns — would show the
// user two identical entries.
func ShellSyncRootRegistered(path string) bool {
	id, err := shellRootID(path)
	if err != nil {
		return false
	}
	for _, hive := range []registry.Key{registry.LOCAL_MACHINE, registry.CURRENT_USER} {
		if k, err := registry.OpenKey(hive, syncRootManager+`\`+id, registry.READ); err == nil {
			k.Close()
			return true
		}
	}
	return false
}

func removeLegacyHKCUEntry(id string) {
	base := syncRootManager + `\` + id
	_ = registry.DeleteKey(registry.CURRENT_USER, base+`\UserSyncRoots`)
	_ = registry.DeleteKey(registry.CURRENT_USER, base)
}

// UnregisterLegacyShellSyncRoot removes the old fixed-id SyncRootManager entry
// ("Nimbo!<SID>!Nimbo") used before per-path ids — a one-time migration cleanup.
func UnregisterLegacyShellSyncRoot() {
	u, err := user.Current()
	if err != nil {
		return
	}
	base := syncRootManager + `\` + "Nimbo!" + u.Uid + "!Nimbo"
	_ = registry.DeleteKey(registry.CURRENT_USER, base+`\UserSyncRoots`)
	_ = registry.DeleteKey(registry.CURRENT_USER, base)
}

// UnregisterShellSyncRoot removes the SyncRootManager metadata for path. Both
// routes are attempted because a given machine may carry an entry from either.
func UnregisterShellSyncRoot(path string) {
	id, err := shellRootID(path)
	if err != nil {
		return
	}
	if err := unregisterShellSyncRootWinRT(id); err != nil {
		slog.Debug("shell sync-root unregister via WinRT failed", "path", path, "err", err)
	}
	removeLegacyHKCUEntry(id)
}

// UnregisterSyncRoot removes the sync-root registration for path.
func UnregisterSyncRoot(path string) error {
	pathW, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	hr, _, _ := procCfUnregisterSyncRoot.Call(uintptr(unsafe.Pointer(pathW)))
	if int32(hr) < 0 {
		return fmt.Errorf("CfUnregisterSyncRoot: 0x%08x", uint32(hr))
	}
	return nil
}

// --- Connect (provider) + placeholders + hydrate-on-open ---

// Debug, if set, receives diagnostic messages from the provider callbacks so
// the hydration path can be observed (and tested) without a debugger.
var Debug func(format string, args ...any)

func dbg(format string, args ...any) {
	if Debug != nil {
		Debug(format, args...)
	}
}

// HydrateFunc returns up to length bytes of the file identified by identity
// starting at offset. identity is the blob set when the placeholder was created
// (we use the UTF-8 remote path).
type HydrateFunc func(identity []byte, offset, length int64) ([]byte, error)

// ListFunc returns the children of a directory (rel is relative to the sync
// root, "" for the root, forward-slash separated) so they can be populated on
// demand.
type ListFunc func(rel string) []PlaceholderInfo

type provider struct {
	path    string
	hydrate HydrateFunc
	list    ListFunc
}

var (
	providers            sync.Map // connKey int64 -> *provider
	fetchDataCallbackPtr = syscall.NewCallback(fetchDataCallback)
	fetchPlaceholdersPtr = syscall.NewCallback(fetchPlaceholdersCallback)
)

// CF_CALLBACK_REGISTRATION { CF_CALLBACK_TYPE Type; CF_CALLBACK Callback; }
type callbackRegistration struct {
	Type     int32
	_        int32
	Callback uintptr
}

const (
	cfCallbackTypeFetchData             = 0
	cfCallbackTypeFetchPlaceholders     = 3
	cfCallbackTypeNone                  = -1
	cfConnectFlagNone                   = 0
	cfConnectFlagRequireProcessInfo     = 2 // CF_CONNECT_FLAG_REQUIRE_PROCESS_INFO
	cfConnectFlagRequireFullFilePath    = 4 // CF_CONNECT_FLAG_REQUIRE_FULL_FILE_PATH
	cfOperationTypeTransferData         = 0
	cfOperationTypeTransferPlaceholders = 4
)

// Mount registers path as a sync root and connects a provider that hydrates
// files (hydrate) and populates directories on demand (list). Returns a
// connection key used to unmount.
func Mount(path, displayName, iconPath string, hydrate HydrateFunc, list ListFunc) (int64, error) {
	// Step-by-step breadcrumbs: a mode switch once went silent for good inside
	// this function on a live install with no stack obtainable (packaged app).
	// If it happens again, the last line printed names the hung call.
	dbg("Mount %q: registering sync root", path)
	if err := RegisterSyncRoot(path); err != nil {
		return 0, err
	}
	dbg("Mount %q: registering shell sync root (COM)", path)
	if err := RegisterShellSyncRoot(path, displayName, iconPath, ShellPolicyOnDemand); err != nil {
		_ = UnregisterSyncRoot(path)
		return 0, err
	}
	dbg("Mount %q: connecting provider", path)
	pathW, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	table := []callbackRegistration{
		{Type: cfCallbackTypeFetchData, Callback: fetchDataCallbackPtr},
		{Type: cfCallbackTypeFetchPlaceholders, Callback: fetchPlaceholdersPtr},
		{Type: cfCallbackTypeNone},
	}
	var connKey int64
	hr, _, _ := procCfConnectSyncRoot.Call(
		uintptr(unsafe.Pointer(pathW)),
		uintptr(unsafe.Pointer(&table[0])),
		0, // callback context
		// Process info names WHO triggered each hydration — without it a
		// "files are re-downloading by themselves" report is undiagnosable.
		uintptr(cfConnectFlagRequireProcessInfo|cfConnectFlagRequireFullFilePath),
		uintptr(unsafe.Pointer(&connKey)),
	)
	runtime.KeepAlive(table)
	if int32(hr) < 0 {
		UnregisterShellSyncRoot(path)
		_ = UnregisterSyncRoot(path)
		return 0, fmt.Errorf("CfConnectSyncRoot: 0x%08x", uint32(hr))
	}
	providers.Store(connKey, &provider{path: path, hydrate: hydrate, list: list})
	dbg("Mount %q: connected (conn=%d), checking root seed", path, connKey)

	// Seed only the root's top level (its on-demand population is disabled).
	// Each subdirectory is created as a not-in-sync placeholder, so the shell
	// issues FETCH_PLACEHOLDERS the first time it's opened — populating lazily
	// and scaling to any account size without crawling it up front. Skip seeding
	// when the folder already has entries (a reconnect of a previous session).
	if list != nil && dirEmpty(path) {
		if items := list(""); len(items) > 0 {
			if err := CreatePlaceholders(path, items); err != nil {
				dbg("seed root: %v", err)
			}
		}
	}
	return connKey, nil
}

// RevertPlaceholder converts a HYDRATED placeholder back into a plain file or
// directory in place (data preserved, placeholder metadata removed). Only valid
// while its sync root is still registered — the leave-VFS revert pass runs
// before Unmount for exactly this reason. A dehydrated placeholder cannot be
// reverted (no bytes to keep); delete it and let sync re-download instead.
func RevertPlaceholder(path string) error {
	h, err := openForCloud(path)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	hr, _, _ := procCfRevertPlaceholder.Call(uintptr(h), 0 /* CF_REVERT_FLAG_NONE */, 0)
	if int32(hr) < 0 {
		return fmt.Errorf("CfRevertPlaceholder: 0x%08x", uint32(hr))
	}
	return nil
}

// dirEmpty reports whether path has no entries (or can't be read).
func dirEmpty(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	names, _ := f.Readdirnames(1)
	return len(names) == 0
}

// Unmount disconnects the provider and unregisters the sync root (filter + shell).
// Unmount permanently stops path being a cloud folder: disconnects the
// provider AND removes the sync-root registration.
//
// UNREGISTERING IS DESTRUCTIVE TO PLACEHOLDER STATE: once the registration is
// gone, Windows quietly strips the cloud state from everything under the root
// (hydrated placeholders revert to plain files). Discovered 2026-08-18 after
// every app update flattened the VM's mount: shutdown used this full Unmount,
// so each restart razed what the previous session built. Use Unmount only when
// the folder should STOP being a cloud folder (leaving virtual-files mode —
// after the deliberate revert pass — or signing out, where the flattening is
// exactly the desired end state). For shutdown and anything temporary, use
// Disconnect: the registration persists across sessions BY DESIGN, exactly as
// OneDrive's does while OneDrive isn't running.
func Unmount(path string, connKey int64) {
	providers.Delete(connKey)
	_, _, _ = procCfDisconnectSyncRoot.Call(uintptr(connKey))
	UnregisterShellSyncRoot(path)
	_ = UnregisterSyncRoot(path)
}

// Disconnect detaches the provider session but leaves the sync-root
// registration (and therefore every placeholder's cloud state) intact. The
// shutdown / pause counterpart to Mount; the next Mount reconnects in place.
func Disconnect(path string, connKey int64) {
	_ = path
	providers.Delete(connKey)
	_, _, _ = procCfDisconnectSyncRoot.Call(uintptr(connKey))
}

// Purge force-removes an on-demand folder together with its placeholders. An
// unregistered-but-on-disk placeholder tree is unusable — the cldflt filter
// rejects every operation with "the cloud file metadata is corrupt and
// unreadable" — so simply unregistering and leaving the files behind strands
// them. Purge instead connects a provider (so the filter services normal
// placeholder deletes), removes the tree, then unmounts; unregistering the sync
// root releases any genuinely-corrupt placeholders that survived, and a final
// sweep clears them. The placeholders' content lives on the server, so removing
// the local copies is non-destructive. Safe to call on a path already gone.
func Purge(path string) error {
	if _, err := os.Stat(path); err != nil {
		// Nothing on disk; just drop any lingering registration.
		UnregisterShellSyncRoot(path)
		_ = UnregisterSyncRoot(path)
		return nil
	}
	hydrate := func(identity []byte, offset, length int64) ([]byte, error) {
		return nil, fmt.Errorf("cfapi: purge (hydration disabled)")
	}
	connKey, err := Mount(path, "Nimbo", "", hydrate, nil)
	if err != nil {
		// A stale registration can block re-register; clear it and retry once.
		UnregisterShellSyncRoot(path)
		_ = UnregisterSyncRoot(path)
		connKey, err = Mount(path, "Nimbo", "", hydrate, nil)
	}
	if err != nil {
		// No provider available; unregister and remove best-effort.
		UnregisterShellSyncRoot(path)
		_ = UnregisterSyncRoot(path)
		return os.RemoveAll(path)
	}
	_ = os.RemoveAll(path) // most placeholders delete with the provider connected
	Unmount(path, connKey) // disconnect + unregister releases corrupt remnants
	return os.RemoveAll(path)
}

// PlaceholderInfo describes one online-only entry to create.
type PlaceholderInfo struct {
	Name     string // entry name relative to the base directory
	Size     int64
	IsDir    bool
	ModTime  time.Time
	Identity []byte // opaque per-file blob (we use the UTF-8 remote path)
	ETag     string // server ETag (carried for the write-back conflict baseline; not stored in the placeholder)
	FileID   string // server oc:fileid — stable across renames; used for down-sync rename detection
}

// CF_FS_METADATA { FILE_BASIC_INFO BasicInfo; LARGE_INTEGER FileSize; }
type fileBasicInfo struct {
	CreationTime   int64
	LastAccessTime int64
	LastWriteTime  int64
	ChangeTime     int64
	FileAttributes uint32
	_              uint32
}
type fsMetadata struct {
	BasicInfo fileBasicInfo
	FileSize  int64
}

// CF_PLACEHOLDER_CREATE_INFO
type placeholderCreateInfo struct {
	RelativeFileName   *uint16
	FsMetadata         fsMetadata
	FileIdentity       uintptr
	FileIdentityLength uint32
	Flags              uint32
	Result             int32
	_                  uint32
	CreateUsn          int64
}

const (
	fileAttrNormal                                   = 0x00000080
	fileAttrDirectory                                = 0x00000010
	cfPlaceholderCreateFlagDisableOnDemandPopulation = 0x00000001
	cfPlaceholderCreateFlagMarkInSync                = 0x00000002
	cfCreateFlagNone                                 = 0
)

func toFiletime(t time.Time) int64 {
	if t.IsZero() {
		t = time.Now()
	}
	return t.UnixNano()/100 + 116444736000000000
}

// buildPlaceholders turns items into the CF_PLACEHOLDER_CREATE_INFO array that
// both CfCreatePlaceholders and CfExecute(TRANSFER_PLACEHOLDERS) consume. The
// returned names/ids slices must be kept alive until the call completes. Entries
// are marked in-sync only — directories are NOT flagged DisableOnDemandPopulation
// so opening them triggers their own FETCH_PLACEHOLDERS (lazy population).
func buildPlaceholders(items []PlaceholderInfo) (arr []placeholderCreateInfo, names [][]uint16, ids [][]byte, err error) {
	arr = make([]placeholderCreateInfo, len(items))
	names = make([][]uint16, len(items))
	ids = make([][]byte, len(items))
	for i, it := range items {
		nameW, e := windows.UTF16FromString(it.Name)
		if e != nil {
			return nil, nil, nil, e
		}
		names[i] = nameW
		ids[i] = it.Identity
		ft := toFiletime(it.ModTime)
		attr := uint32(fileAttrNormal)
		flags := uint32(cfPlaceholderCreateFlagMarkInSync)
		if it.IsDir {
			attr = fileAttrDirectory
			// A not-in-sync directory is treated as not-yet-populated, so the
			// shell issues FETCH_PLACEHOLDERS when it's first opened.
			flags = cfCreateFlagNone
		}
		arr[i] = placeholderCreateInfo{
			RelativeFileName: &nameW[0],
			FsMetadata: fsMetadata{
				BasicInfo: fileBasicInfo{CreationTime: ft, LastWriteTime: ft, LastAccessTime: ft, ChangeTime: ft, FileAttributes: attr},
				FileSize:  it.Size,
			},
			Flags: flags,
		}
		if len(it.Identity) > 0 {
			arr[i].FileIdentity = uintptr(unsafe.Pointer(&ids[i][0]))
			arr[i].FileIdentityLength = uint32(len(ids[i]))
		}
	}
	return arr, names, ids, nil
}

// CreatePlaceholders creates online-only entries directly under baseDir (used by
// tests/probes; the live provider populates lazily via FETCH_PLACEHOLDERS).
func CreatePlaceholders(baseDir string, items []PlaceholderInfo) error {
	if len(items) == 0 {
		return nil
	}
	baseW, err := windows.UTF16PtrFromString(baseDir)
	if err != nil {
		return err
	}
	arr, names, ids, err := buildPlaceholders(items)
	if err != nil {
		return err
	}
	var processed uint32
	hr, _, _ := procCfCreatePlaceholders.Call(
		uintptr(unsafe.Pointer(baseW)),
		uintptr(unsafe.Pointer(&arr[0])),
		uintptr(len(arr)),
		uintptr(cfCreateFlagNone),
		uintptr(unsafe.Pointer(&processed)),
	)
	runtime.KeepAlive(names)
	runtime.KeepAlive(ids)
	runtime.KeepAlive(arr)
	if int32(hr) < 0 {
		return fmt.Errorf("CfCreatePlaceholders: 0x%08x (processed %d/%d)", uint32(hr), processed, len(arr))
	}
	return nil
}

// --- FETCH_DATA callback + CfExecute(TRANSFER_DATA) ---

// CF_CALLBACK_INFO field offsets (amd64), translated field-by-field from the
// cfapi.h struct so the layout is exact rather than guessed:
//
//	StructSize(4)+pad@0  ConnectionKey@8  CallbackContext@16  VolumeGuidName@24
//	VolumeDosName@32  VolumeSerialNumber(4)+pad@40  SyncRootFileId@48
//	SyncRootIdentity@56  SyncRootIdentityLength(4)+pad@64  FileId@72  FileSize@80
//	FileIdentity@88  FileIdentityLength(4)+pad@96  NormalizedPath@104
//	TransferKey@112  ...
const (
	ciConnectionKey      = 8
	ciFileSize           = 80
	ciFileIdentity       = 88
	ciFileIdentityLength = 96
	ciNormalizedPath     = 104
	ciTransferKey        = 112
	// PriorityHint(1)+pad@120  CorrelationVector@128  ProcessInfo@136  RequestKey@144
	ciProcessInfo = 136
	// CF_PROCESS_INFO field offsets (amd64): StructSize@0 ProcessId@4
	// ImagePath@8 PackageName@16 ApplicationId@24 CommandLine@32 SessionId@40.
	piProcessId   = 4
	piImagePath   = 8
	piCommandLine = 32
	// CF_CALLBACK_PARAMETERS (FetchData) offsets.
	cpRequiredOffset = 16
	cpRequiredLength = 24
)

// callbackProcess names the process whose access triggered a callback, for
// hydration forensics ("who is re-downloading freed files?"). Requires the
// connection to pass CF_CONNECT_FLAG_REQUIRE_PROCESS_INFO; returns "" when the
// filter supplied none.
func callbackProcess(info uintptr) string {
	pi := *(*uintptr)(unsafe.Pointer(info + ciProcessInfo))
	if pi == 0 {
		return ""
	}
	pid := *(*uint32)(unsafe.Pointer(pi + piProcessId))
	imgPtr := *(*uintptr)(unsafe.Pointer(pi + piImagePath))
	img := ""
	if imgPtr != 0 {
		img = windows.UTF16PtrToString((*uint16)(unsafe.Pointer(imgPtr)))
	}
	cmdPtr := *(*uintptr)(unsafe.Pointer(pi + piCommandLine))
	cmd := ""
	if cmdPtr != 0 {
		cmd = windows.UTF16PtrToString((*uint16)(unsafe.Pointer(cmdPtr)))
	}
	return fmt.Sprintf("pid=%d %s cmd=%q", pid, img, cmd)
}

// fetchDataCallback MUST return quickly: it runs on a thread the OS/Explorer is
// waiting on, so blocking here (e.g. a network download) freezes Explorer. We
// copy the request and fulfil it on a background goroutine, completing the
// transfer via the TransferKey (cfapi allows CfExecute from any thread).
func fetchDataCallback(info, params uintptr) uintptr {
	connKey := *(*int64)(unsafe.Pointer(info + ciConnectionKey))
	transferKey := *(*int64)(unsafe.Pointer(info + ciTransferKey))
	idPtr := *(*uintptr)(unsafe.Pointer(info + ciFileIdentity))
	idLen := *(*uint32)(unsafe.Pointer(info + ciFileIdentityLength))
	reqOffset := *(*int64)(unsafe.Pointer(params + cpRequiredOffset))
	reqLength := *(*int64)(unsafe.Pointer(params + cpRequiredLength))
	idStr := ""
	if idPtr != 0 && idLen > 0 {
		raw := unsafe.Slice((*byte)(unsafe.Pointer(idPtr)), idLen)
		idStr = string(raw) // identities are the remote path in UTF-8
	}
	dbg("FETCH_DATA id=%q offset=%d length=%d by=%s", idStr, reqOffset, reqLength, callbackProcess(info))

	pv, ok := providers.Load(connKey)
	if !ok {
		dbg("FETCH_DATA: no provider for conn=%d", connKey)
		return 0
	}
	hyd := pv.(*provider).hydrate

	identity := make([]byte, idLen) // copy — the OS buffer is only valid during the callback
	if idLen > 0 {
		copy(identity, unsafe.Slice((*byte)(unsafe.Pointer(idPtr)), idLen))
	}

	go func() {
		const chunk = 1 << 20
		off := reqOffset
		remaining := reqLength
		for remaining > 0 {
			n := int64(chunk)
			if n > remaining {
				n = remaining
			}
			data, err := hyd(identity, off, n)
			if err != nil || len(data) == 0 {
				return // incomplete transfer → the open fails, but Explorer isn't blocked
			}
			cfTransfer(connKey, transferKey, off, data)
			off += int64(len(data))
			remaining -= int64(len(data))
		}
	}()
	return 0
}

// CF_OPERATION_INFO
type operationInfo struct {
	StructSize    uint32
	Type          uint32
	ConnectionKey int64
	TransferKey   int64
	// Field ORDER is load-bearing (cfapi.h: CorrelationVector, SyncStatus,
	// RequestKey — pinned by TestStructLayoutsMatchCfapiH). All three are
	// currently always zero, but with RequestKey first a future assignment
	// would be read by the filter as a CORRELATION_VECTOR pointer.
	CorrelationVector uintptr
	SyncStatus        uintptr
	RequestKey        int64
}

// CF_OPERATION_PARAMETERS (TransferData).
//
// LAYOUT IS LOAD-BEARING: ParamSize is followed by a UNION whose members hold
// 8-byte fields, so the union starts at offset 8 — there are FOUR PADDING BYTES
// after ParamSize. The original declaration omitted them and put Flags at
// offset 4: the filter then read Flags from where CompletionStatus sat (zero,
// harmlessly) — and in TRANSFER_PLACEHOLDERS the same slip meant the
// DISABLE_ON_DEMAND_POPULATION completion flag NEVER reached the filter, so no
// directory populated through FETCH_PLACEHOLDERS was ever marked complete and
// the filter re-requested it on every enumeration, forever: the #569 fetch
// storm (256 root fetches in 5s in the harness; ~9/s in the field with each
// one a server PROPFIND). Verified against cfapi.h 10.0.26100.
type opParamsTransfer struct {
	ParamSize        uint32
	_                uint32 // union alignment — see above
	Flags            uint32
	CompletionStatus int32
	Buffer           uintptr
	Offset           int64
	Length           int64
}

func cfTransfer(connKey, transferKey, offset int64, data []byte) {
	oi := operationInfo{Type: cfOperationTypeTransferData, ConnectionKey: connKey, TransferKey: transferKey}
	oi.StructSize = uint32(unsafe.Sizeof(oi))
	op := opParamsTransfer{
		Buffer: uintptr(unsafe.Pointer(&data[0])),
		Offset: offset,
		Length: int64(len(data)),
	}
	op.ParamSize = uint32(unsafe.Sizeof(op))
	hr, _, _ := procCfExecute.Call(uintptr(unsafe.Pointer(&oi)), uintptr(unsafe.Pointer(&op)))
	runtime.KeepAlive(data)
	if int32(hr) < 0 {
		dbg("CfExecute(TRANSFER_DATA) offset=%d len=%d -> 0x%08x", offset, len(data), uint32(hr))
	} else {
		dbg("CfExecute(TRANSFER_DATA) offset=%d len=%d -> ok", offset, len(data))
	}
}

// --- FETCH_PLACEHOLDERS callback + CfExecute(TRANSFER_PLACEHOLDERS) ---
//
// When Explorer enumerates a sync-root directory whose contents haven't been
// populated yet (partial population policy), the filter issues a
// FETCH_PLACEHOLDERS request. We look up the directory's children via the
// provider's ListFunc and feed them back as placeholders, completing the
// operation so Explorer renders the folder instead of timing out.

// readUTF16 reads a NUL-terminated UTF-16 string from a pointer (or "" if nil).
func readUTF16(ptr uintptr) string {
	if ptr == 0 {
		return ""
	}
	return windows.UTF16PtrToString((*uint16)(unsafe.Pointer(ptr)))
}

// relFromNormalized derives the sync-root-relative, forward-slash path of the
// directory being populated. NormalizedPath is volume-relative with NO drive
// letter (e.g. `\Users\Adam\Desktop\test1\sub`), so we drop the sync root's
// drive before matching, then strip the sync-root prefix.
func relFromNormalized(syncRoot, normalized string) string {
	root := syncRoot
	if len(root) >= 2 && root[1] == ':' {
		root = root[2:] // "C:\Users\..." -> "\Users\..."
	}
	p := normalized
	lp, lr := strings.ToLower(p), strings.ToLower(root)
	if i := strings.Index(lp, lr); i >= 0 {
		p = p[i+len(root):]
	}
	p = strings.ReplaceAll(p, `\`, "/")
	return strings.Trim(p, "/")
}

func fetchPlaceholdersCallback(info, params uintptr) uintptr {
	connKey := *(*int64)(unsafe.Pointer(info + ciConnectionKey))
	transferKey := *(*int64)(unsafe.Pointer(info + ciTransferKey))
	normalized := readUTF16(*(*uintptr)(unsafe.Pointer(info + ciNormalizedPath)))
	dbg("FETCH_PLACEHOLDERS conn=%d transfer=%d path=%q", connKey, transferKey, normalized)

	pv, ok := providers.Load(connKey)
	if !ok {
		dbg("FETCH_PLACEHOLDERS: no provider for conn=%d", connKey)
		return 0
	}
	p := pv.(*provider)
	if p.list == nil {
		return 0
	}
	rel := relFromNormalized(p.path, normalized)

	syncDone := make(chan struct{})
	go func() {
		defer close(syncDone)
		items := p.list(rel)
		dbg("FETCH_PLACEHOLDERS rel=%q -> %d entries", rel, len(items))
		cfTransferPlaceholders(connKey, transferKey, items)
		// Deliberately NOT marked in-sync here. Marking the directory while the
		// enumeration that triggered this fetch is still in flight makes that
		// enumeration return EMPTY (measured on the live driver — the first
		// attempt at Deck #576 did exactly this and TestStateBitsAcrossLifecycle
		// caught it). SweepDirsInSync gives populated directories their in-sync
		// state later, once they have been quiet for a while.
	}()
	if experimentSyncFetch {
		<-syncDone // EXPERIMENT: complete before the callback returns
	}
	return 0
}

// experimentSyncFetch is a diagnostic toggle: when true, the FETCH_PLACEHOLDERS
// callback blocks until the transfer has been executed instead of returning
// immediately and completing asynchronously.
var experimentSyncFetch = os.Getenv("NIMBO_CFAPI_SYNC_FETCH") == "1"

// CF_OPERATION_PARAMETERS (TransferPlaceholders). Same union-alignment rule as
// opParamsTransfer — the padding sits after ParamSize, NOT in the middle. With
// the old (shifted) layout every field from Flags onward happened to land
// correctly EXCEPT Flags and CompletionStatus, which is why placeholders were
// delivered fine while the populated-flag silently vanished.
type opParamsTransferPlaceholders struct {
	ParamSize             uint32
	_                     uint32 // union alignment
	Flags                 uint32
	CompletionStatus      int32
	PlaceholderTotalCount int64
	PlaceholderArray      uintptr
	PlaceholderCount      uint32
	EntriesProcessed      uint32
}

// 0x1 is STOP_ON_ERROR; DISABLE_ON_DEMAND_POPULATION (mark the dir fully
// populated so the shell stops re-requesting it) is 0x2.
const cfOpTransferPlaceholdersFlagDisableOnDemandPopulation = 0x00000002

// cfTransferPlaceholders delivers items and reports whether the transfer was
// accepted, so the caller can mark the directory in-sync only on success.
func cfTransferPlaceholders(connKey, transferKey int64, items []PlaceholderInfo) bool {
	arr, names, ids, err := buildPlaceholders(items)
	if err != nil {
		dbg("TRANSFER_PLACEHOLDERS build error: %v", err)
		return false
	}
	oi := operationInfo{Type: cfOperationTypeTransferPlaceholders, ConnectionKey: connKey, TransferKey: transferKey}
	oi.StructSize = uint32(unsafe.Sizeof(oi))
	// Mark the directory fully populated ONLY when the listing actually
	// succeeded. A nil slice means the provider could not reach the server; an
	// empty non-nil slice means the folder is genuinely empty.
	//
	// This distinction matters enormously: the flag is permanent. Setting it
	// after a failed listing tells Windows the folder is complete and empty, and
	// it then NEVER asks again — not after a restart, not after leaving and
	// re-entering virtual-files mode, because a revert only touches placeholders
	// and there is nothing there to revert. Seen in the field 2026-08-16: a
	// shared folder stuck empty with its file present on the server.
	flags := uint32(cfOpTransferPlaceholdersFlagDisableOnDemandPopulation)
	if items == nil {
		flags = 0 // listing failed — leave it unpopulated so the shell retries
		dbg("TRANSFER_PLACEHOLDERS: listing failed, not marking %s populated", "dir")
	}
	op := opParamsTransferPlaceholders{
		Flags:                 flags,
		PlaceholderTotalCount: int64(len(arr)),
		PlaceholderCount:      uint32(len(arr)),
	}
	op.ParamSize = uint32(unsafe.Sizeof(op))
	if len(arr) > 0 {
		op.PlaceholderArray = uintptr(unsafe.Pointer(&arr[0]))
	}
	hr, _, _ := procCfExecute.Call(uintptr(unsafe.Pointer(&oi)), uintptr(unsafe.Pointer(&op)))
	runtime.KeepAlive(names)
	runtime.KeepAlive(ids)
	runtime.KeepAlive(arr)
	if int32(hr) < 0 {
		dbg("CfExecute(TRANSFER_PLACEHOLDERS) count=%d -> 0x%08x", len(arr), uint32(hr))
	} else {
		dbg("CfExecute(TRANSFER_PLACEHOLDERS) count=%d -> ok", len(arr))
	}
	// Windows reports PER-ENTRY success in each placeholder's Result field, and
	// CfExecute can return S_OK while an individual entry failed. An entry that
	// fails is never created, so the directory is never fully populated and the
	// shell re-issues FETCH_PLACEHOLDERS forever — a spin loop seen in the field
	// on 2026-08-14 at ~9 requests/second. Without this line the failure is
	// invisible: the summary above says "ok" either way.
	entriesOK := true
	for i := range arr {
		if arr[i].Result < 0 {
			entriesOK = false
			name := ""
			if i < len(items) {
				name = items[i].Name
			}
			dbg("TRANSFER_PLACEHOLDERS entry %q -> 0x%08x (not created)", name, uint32(arr[i].Result))
		}
	}
	// Success for the caller means the whole delivery took: a failed entry
	// leaves the directory partially populated with the shell re-requesting it,
	// and a directory in that state must NOT be marked in-sync (an in-sync
	// directory is never asked to populate — see TestInSyncDirStillPopulates).
	return int32(hr) >= 0 && entriesOK
}

// --- Write-back: detect user changes + mark synced after upload ---
//
// CF_PLACEHOLDER_STATE bit flags returned by CfGetPlaceholderStateFromAttributeTag.
const (
	cfPlaceholderStatePlaceholder = 0x00000001
	cfPlaceholderStateInSync      = 0x00000008
	cfPlaceholderStateInvalid     = 0xffffffff
)

const (
	cfConvertFlagMarkInSync = 0x00000001
	cfInSyncStateInSync     = 1
	cfSetInSyncFlagNone     = 0
)

// findAttrTag returns a path's file attributes and reparse tag (0 if not a
// reparse point), via FindFirstFile.
func findAttrTag(path string) (attrs, tag uint32, err error) {
	pathW, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var fd windows.Win32finddata
	h, err := windows.FindFirstFile(pathW, &fd)
	if err != nil {
		return 0, 0, err
	}
	windows.FindClose(h)
	tag = 0
	if fd.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		tag = fd.Reserved0 // reparse tag lives here when REPARSE_POINT is set
	}
	return fd.FileAttributes, tag, nil
}

// Change describes how the local file at a path differs from the server, for
// the on-demand write-back watcher.
type Change struct {
	IsDir       bool
	NeedsUpload bool // user created/modified content that should be pushed up
}

// Inspect classifies path for write-back: a dirty placeholder (user edited a
// hydrated file) or a brand-new non-placeholder file/dir needs uploading; a
// clean in-sync placeholder (incl. one we just hydrated) does not.
func Inspect(path string) (Change, error) {
	attrs, tag, err := findAttrTag(path)
	if err != nil {
		return Change{}, err
	}
	isDir := attrs&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	r1, _, _ := procCfGetPlaceholderStateFromAttrTag.Call(uintptr(attrs), uintptr(tag))
	state := uint32(r1)
	if state == cfPlaceholderStateInvalid {
		state = 0
	}
	isPlaceholder := state&cfPlaceholderStatePlaceholder != 0
	inSync := state&cfPlaceholderStateInSync != 0
	var need bool
	if isDir {
		// Our directory placeholders are deliberately NOT in-sync (that's how
		// lazy FETCH_PLACEHOLDERS population is triggered), so "not in sync" must
		// NOT be read as a change. Only a non-placeholder dir is a folder the
		// user just created and needs MKCOL.
		need = !isPlaceholder
	} else {
		// A file needs upload when it's non-placeholder (freshly created) or a
		// placeholder whose in-sync flag was cleared (edited after hydration).
		// A clean in-sync placeholder — including one we just hydrated — is
		// skipped.
		need = !(isPlaceholder && inSync)
	}
	return Change{IsDir: isDir, NeedsUpload: need}, nil
}

// MarkInSync records that path now matches the server: a regular file/dir is
// converted to an in-sync placeholder (identity = remote path); an existing
// placeholder just has its in-sync state set. Call after a successful upload so
// the watcher won't re-upload it.
func MarkInSync(path string, identity []byte) error {
	attrs, tag, err := findAttrTag(path)
	if err != nil {
		return err
	}
	flag := uint32(windows.FILE_FLAG_BACKUP_SEMANTICS) // needed to open directories
	pathW, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(pathW,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, flag, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)

	r1, _, _ := procCfGetPlaceholderStateFromAttrTag.Call(uintptr(attrs), uintptr(tag))
	isPlaceholder := uint32(r1)&cfPlaceholderStatePlaceholder != 0 && uint32(r1) != cfPlaceholderStateInvalid

	var usn int64
	if isPlaceholder {
		hr, _, _ := procCfSetInSyncState.Call(uintptr(h), uintptr(cfInSyncStateInSync), uintptr(cfSetInSyncFlagNone), uintptr(unsafe.Pointer(&usn)))
		if int32(hr) < 0 {
			return fmt.Errorf("CfSetInSyncState: 0x%08x", uint32(hr))
		}
		return nil
	}
	var idPtr uintptr
	if len(identity) > 0 {
		idPtr = uintptr(unsafe.Pointer(&identity[0]))
	}
	hr, _, _ := procCfConvertToPlaceholder.Call(
		uintptr(h), idPtr, uintptr(len(identity)),
		uintptr(cfConvertFlagMarkInSync), uintptr(unsafe.Pointer(&usn)), 0)
	runtime.KeepAlive(identity)
	if uint32(hr) == 0x8007017C { // ERROR_CLOUD_FILE_INVALID_REQUEST
		// Already a placeholder — our is-placeholder probe above was DISGUISED
		// (this process is not a connected sync engine for the root, so the
		// filter hides reparse detail; see the disguising GOTCHA at the cldapi
		// proc table). The laptop's live-mode walk hit this 121k times on its
		// second pass. The file exists and is ours: just (re)assert in-sync.
		hr2, _, _ := procCfSetInSyncState.Call(uintptr(h), uintptr(cfInSyncStateInSync), uintptr(cfSetInSyncFlagNone), uintptr(unsafe.Pointer(&usn)))
		if int32(hr2) < 0 {
			return fmt.Errorf("CfSetInSyncState after disguised re-convert: 0x%08x", uint32(hr2))
		}
		return nil
	}
	if int32(hr) < 0 {
		return fmt.Errorf("CfConvertToPlaceholder: 0x%08x", uint32(hr))
	}
	return nil
}

// --- Deferred in-sync marking for populated directories (Deck #576) ---

// cfPlaceholderStatePartial marks a placeholder whose content is not fully
// present: for a directory, "not yet populated".
const cfPlaceholderStatePartial = 0x00000010

// SweepDirsInSync walks an on-demand mount and gives every POPULATED directory
// its in-sync state, so Explorer stops showing the perpetual "sync pending"
// arrows on folders. Returns how many directories it marked.
//
// Why this is a sweep and not part of population — all measured on the live
// driver and pinned by tests in this package:
//   - A directory placeholder cannot be created in-sync: the filter then never
//     asks it to populate at all, and it enumerates empty forever
//     (TestInSyncDirStillPopulates).
//   - It cannot be marked immediately after its population transfer either:
//     that poisons the very enumeration that triggered the fetch, which then
//     returns empty once (TestStateBitsAcrossLifecycle caught this).
//
// So populated directories are marked LATER, from here. Safety rules:
//   - Only directories whose PARTIAL bit is clear (population complete —
//     a failed listing leaves the bit set, so those are skipped and the shell
//     keeps retrying them).
//   - Only after quiesce of write inactivity, so a directory whose triggering
//     enumeration might still be draining is left for the next pass.
//
// The sweep also HEALS mounts created before this existed: every directory a
// user ever opened is populated-but-unmarked on those, permanently, because
// population never fires twice.
func SweepDirsInSync(root string, quiesce time.Duration) int {
	marked := 0
	cutoff := time.Now().Add(-quiesce)
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() || path == root {
			return nil
		}
		attrs, tag, ferr := findAttrTag(path)
		if ferr != nil {
			return nil
		}
		r1, _, _ := procCfGetPlaceholderStateFromAttrTag.Call(uintptr(attrs), uintptr(tag))
		state := uint32(r1)
		if state == cfPlaceholderStateInvalid {
			return nil
		}
		if state&cfPlaceholderStatePlaceholder == 0 || state&cfPlaceholderStateInSync != 0 {
			return nil // not ours, or already correct
		}
		if state&cfPlaceholderStatePartial != 0 {
			return nil // not (successfully) populated yet — marking would freeze it empty
		}
		if info, ierr := d.Info(); ierr == nil && info.ModTime().After(cutoff) {
			return nil // recently active; let it settle until the next sweep
		}
		if merr := MarkInSync(path, nil); merr == nil {
			ShellNotifyUpdated(path) // redraw the folder glyph without a manual refresh
			marked++
		} else {
			dbg("SweepDirsInSync %q: %v", path, merr)
		}
		return nil
	})
	return marked
}

// SettlePin completes a pending free-up-space request on one FILE: Explorer's
// "Free up space" verb only sets FILE_ATTRIBUTE_UNPINNED and waits for the
// provider to dehydrate — until the provider does, the item (and every
// ancestor directory) wears the sync-pending arrows, truthfully but forever.
// Returns true when it dehydrated the file.
//
// Only an unpinned + hydrated + IN-SYNC file is eligible: a dirty file's data
// is not yet on the server, and dehydrating it would destroy the only copy
// (the platform refuses that anyway — ERROR_CLOUD_FILE_NOT_IN_SYNC — but we
// don't rely on it). Directories carry the unpinned attribute purely as the
// recursive preference marker and are never touched.
func SettlePin(path string) (bool, error) {
	attrs, tag, err := findAttrTag(path)
	if err != nil {
		return false, err
	}
	if attrs&fileAttrDirectory != 0 || attrs&fileAttrUnpinned == 0 || attrs&0x400000 != 0 {
		return false, nil // a dir, not unpinned, or already dehydrated
	}
	r1, _, _ := procCfGetPlaceholderStateFromAttrTag.Call(uintptr(attrs), uintptr(tag))
	state := uint32(r1)
	if state == cfPlaceholderStateInvalid || state&cfPlaceholderStatePlaceholder == 0 {
		// A LOCAL-ONLY file (no placeholder, no cloud copy — the sync-excluded
		// journals, Desktop.ini): freeing it is impossible, the disk copy is
		// the only copy. The request is void — clear the attribute so the item
		// stops wearing pending arrows forever.
		pathW, werr := windows.UTF16PtrFromString(path)
		if werr != nil {
			return false, werr
		}
		if err := windows.SetFileAttributes(pathW, attrs&^uint32(fileAttrUnpinned)); err != nil {
			return false, err
		}
		ShellNotifyUpdated(path)
		return true, nil
	}
	if state&cfPlaceholderStateInSync == 0 {
		return false, nil // dirty: its data is not on the server yet — never dehydrate
	}
	if err := Dehydrate(path); err != nil {
		return false, err
	}
	ShellNotifyUpdated(path)
	return true, nil
}

// ExcludeFromSync marks a LOCAL-ONLY file as deliberately outside sync:
// converted to an in-place placeholder (content untouched, never fetched — the
// data is already local and there is no cloud copy) and pinned
// CF_PIN_STATE_EXCLUDED, the platform's vocabulary for "not synced, on
// purpose". Explorer then renders a BLANK Status cell (VM-verified) instead of
// the forever-pending arrows a plain file gets inside a cloud root. For the
// sync-excluded artifacts every takeover inherits: the official client's
// .sync_*.db / .nextcloudsync.log, Desktop.ini. Idempotent.
func ExcludeFromSync(path string) error {
	attrs, tag, err := findAttrTag(path)
	if err != nil {
		return err
	}
	if attrs&fileAttrDirectory != 0 {
		return nil // files only
	}
	r1, _, _ := procCfGetPlaceholderStateFromAttrTag.Call(uintptr(attrs), uintptr(tag))
	if s := uint32(r1); s != cfPlaceholderStateInvalid && s&cfPlaceholderStatePlaceholder != 0 {
		return nil // already converted (and excluded on the first pass)
	}
	if err := MarkInSync(path, []byte("local-only")); err != nil {
		return err
	}
	h, err := openForCloud(path)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	const cfPinStateExcluded = 3
	hr, _, _ := procCfSetPinState.Call(uintptr(h), uintptr(cfPinStateExcluded), 0, 0)
	if int32(hr) < 0 {
		return fmt.Errorf("CfSetPinState(excluded): 0x%08x", uint32(hr))
	}
	ShellNotifyUpdated(path)
	return nil
}

// SweepSettlePins walks an on-demand mount and settles every quiet pending
// free-up-space request (see SettlePin). Returns how many files it
// dehydrated. This is the backstop for requests made while the app wasn't
// running; the write-back watcher settles live ones promptly.
func SweepSettlePins(root string, quiesce time.Duration) int {
	settled := 0
	cutoff := time.Now().Add(-quiesce)
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || path == root || d.IsDir() {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil && info.ModTime().After(cutoff) {
			return nil // recently active; let it settle until the next sweep
		}
		ok, serr := SettlePin(path)
		if serr != nil {
			dbg("SweepSettlePins %q: %v", path, serr)
			return nil
		}
		if ok {
			settled++
		}
		return nil
	})
	return settled
}

// CF_UPDATE_FLAG_MARK_IN_SYNC is 0x2; CF_UPDATE_FLAG_VERIFY_IN_SYNC (0x1)
// makes the update fail when the file isn't already in sync — wrong after a
// rename, exactly right for a refresh that must not clobber a racing edit.
const (
	cfUpdateFlagVerifyInSync = 0x00000001
	cfUpdateFlagMarkInSync   = 0x00000002
)

// UpdateIdentity rewrites a placeholder's file identity (used after a rename so
// hydration fetches the file from its new remote path) and keeps it in-sync.
func UpdateIdentity(path string, identity []byte) error {
	pathW, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(pathW,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	var idPtr uintptr
	if len(identity) > 0 {
		idPtr = uintptr(unsafe.Pointer(&identity[0]))
	}
	var usn int64
	hr, _, _ := procCfUpdatePlaceholder.Call(
		uintptr(h),
		0, // FsMetadata (unchanged)
		idPtr, uintptr(len(identity)),
		0, 0, // no dehydrate ranges
		uintptr(cfUpdateFlagMarkInSync),
		uintptr(unsafe.Pointer(&usn)),
		0, // Overlapped
	)
	runtime.KeepAlive(identity)
	if int32(hr) < 0 {
		return fmt.Errorf("CfUpdatePlaceholder: 0x%08x", uint32(hr))
	}
	return nil
}

// --- Pin / free-up-space (CfSetPinState + CfDehydratePlaceholder) ---

const (
	cfPinStateUnspecified = 0 // CF_PIN_STATE_UNSPECIFIED — no user preference
	cfPinStatePinned      = 1 // CF_PIN_STATE_PINNED — always keep on device
	cfPinStateUnpinned    = 2 // CF_PIN_STATE_UNPINNED — online-only preference
	cfSetPinFlagRecurse   = 1 // CF_SET_PIN_FLAG_RECURSE — apply to a directory tree
	cfDehydrateFlagNone = 0
)

// openForCloud opens a handle suitable for cloud-state operations (BACKUP_SEMANTICS
// so directories can be opened too).
func openForCloud(path string) (windows.Handle, error) {
	pathW, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(pathW,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
}

// FILE_ATTRIBUTE_PINNED / FILE_ATTRIBUTE_UNPINNED — the user's explicit
// availability preference, surfaced in the file's attributes.
const (
	fileAttrPinned   = 0x00080000
	fileAttrUnpinned = 0x00100000
)

// PinStateOf reports a path's explicit availability preference: "pinned"
// (always keep on this device), "unpinned" (online-only preference), or ""
// (no explicit choice — inherits from the parent).
func PinStateOf(path string) string {
	attrs, _, err := findAttrTag(path)
	if err != nil {
		return ""
	}
	if attrs&fileAttrPinned != 0 {
		return "pinned"
	}
	if attrs&fileAttrUnpinned != 0 {
		return "unpinned"
	}
	return ""
}

// SetPinState pins (always keep on device — auto-hydrates and never dehydrates)
// or unpins (online-only preference) a file or directory. recurse applies it to
// a whole directory tree.
func SetPinState(path string, pinned, recurse bool) error {
	h, err := openForCloud(path)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	state := cfPinStateUnpinned
	if pinned {
		state = cfPinStatePinned
	}
	flags := 0
	if recurse {
		flags = cfSetPinFlagRecurse
	}
	hr, _, _ := procCfSetPinState.Call(uintptr(h), uintptr(state), uintptr(flags), 0)
	if int32(hr) < 0 {
		return fmt.Errorf("CfSetPinState: 0x%08x", uint32(hr))
	}
	return nil
}

// Dehydrate drops a hydrated file's local content (keeping the online-only
// placeholder), freeing disk space. Only valid on files; a no-op/err on an
// already-online-only file is harmless to ignore.
func Dehydrate(path string) error {
	// Already dehydrated = done. This guard is NOT an optimisation:
	// CfDehydratePlaceholder on a dataless placeholder makes the filter FETCH
	// the full content first and then discard it (measured live,
	// TestDehydrateAlreadyDehydratedDoesNotFetch) — so a blind re-dehydrate of
	// a freed tree re-downloads every file just to throw it away.
	if attrs, _, aerr := findAttrTag(path); aerr == nil && attrs&0x400000 != 0 {
		return nil // RECALL_ON_DATA_ACCESS: no local data to drop
	}
	h, err := openForCloud(path)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	// StartingOffset 0, Length -1 (to EOF), passed by value as LARGE_INTEGER.
	hr, _, _ := procCfDehydratePlaceholder.Call(uintptr(h), 0, ^uintptr(0), uintptr(cfDehydrateFlagNone), 0)
	if int32(hr) < 0 {
		return fmt.Errorf("CfDehydratePlaceholder: 0x%08x", uint32(hr))
	}
	return nil
}

// RefreshPlaceholder updates an in-sync placeholder to a changed server version:
// it drops any stale local content (so the next open re-fetches) and updates the
// logical size/mtime, keeping it in-sync. identity is the remote path (unchanged).
func RefreshPlaceholder(path string, identity []byte, size int64, mtime time.Time) error {
	return refreshPlaceholder(path, identity, size, mtime, cfUpdateFlagMarkInSync)
}

// ErrNotInSync reports that a verify-gated placeholder update was refused
// because the placeholder is no longer IN SYNC — i.e. a local edit landed
// after the caller's dirty check. The caller should skip, not fail.
var ErrNotInSync = errors.New("placeholder is no longer in sync")

// RefreshPlaceholderIfInSync is RefreshPlaceholder gated on the placeholder
// still being IN SYNC at update time (CF_UPDATE_FLAG_VERIFY_IN_SYNC): a local
// edit racing the caller's dirty-check makes this return ErrNotInSync instead
// of the refresh dehydrating the edit's only copy away.
func RefreshPlaceholderIfInSync(path string, identity []byte, size int64, mtime time.Time) error {
	return refreshPlaceholder(path, identity, size, mtime, cfUpdateFlagMarkInSync|cfUpdateFlagVerifyInSync)
}

func refreshPlaceholder(path string, identity []byte, size int64, mtime time.Time, flags uint32) error {
	h, err := openForCloud(path)
	if err != nil {
		return err
	}
	ft := toFiletime(mtime)
	meta := fsMetadata{
		BasicInfo: fileBasicInfo{CreationTime: ft, LastWriteTime: ft, LastAccessTime: ft, ChangeTime: ft, FileAttributes: fileAttrNormal},
		FileSize:  size,
	}
	var idPtr uintptr
	if len(identity) > 0 {
		idPtr = uintptr(unsafe.Pointer(&identity[0]))
	}
	var usn int64
	// Update the logical size/mtime first, then dehydrate — so the WHOLE new
	// range is marked not-present and re-fetched on next open (dehydrating first
	// would only mark the old range, leaving the grown tail as stale zeros).
	hr, _, _ := procCfUpdatePlaceholder.Call(
		uintptr(h),
		uintptr(unsafe.Pointer(&meta)),
		idPtr, uintptr(len(identity)),
		0, 0,
		uintptr(flags),
		uintptr(unsafe.Pointer(&usn)),
		0,
	)
	runtime.KeepAlive(identity)
	runtime.KeepAlive(meta)
	windows.CloseHandle(h)
	if uint32(hr) == 0x80070179 { // ERROR_CLOUD_FILE_NOT_IN_SYNC
		return ErrNotInSync
	}
	if int32(hr) < 0 {
		return fmt.Errorf("CfUpdatePlaceholder(refresh): 0x%08x", uint32(hr))
	}
	_ = Dehydrate(path) // drop all local content so the next open re-fetches it
	return nil
}

// Supported reports whether the Cloud Files API is available (Windows 10 1709+).
func Supported() bool {
	return cldapi.Load() == nil && procCfRegisterSyncRoot.Find() == nil
}

// IsPlaceholder reports whether path currently carries cloud-filter placeholder
// state at all (in whatever hydration/sync state). A plain file or directory —
// e.g. one stripped by a placeholder revert — returns false.
func IsPlaceholder(path string) (bool, error) {
	attrs, tag, err := findAttrTag(path)
	if err != nil {
		return false, err
	}
	r1, _, _ := procCfGetPlaceholderStateFromAttrTag.Call(uintptr(attrs), uintptr(tag))
	state := uint32(r1)
	if state == cfPlaceholderStateInvalid {
		return false, nil
	}
	return state&cfPlaceholderStatePlaceholder != 0, nil
}
