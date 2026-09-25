//go:build windows

// Package cfapi is an experimental integration with the Windows Cloud Files API
// (cldapi.dll) that powers on-demand ("online-only") files. This first layer
// registers and unregisters a folder as a cloud sync root — the foundation the
// placeholder + hydration layers build on. It is opt-in and non-destructive:
// registering a sync root does not alter the files inside it.
package cfapi

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
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
	procCfGetPlaceholderInfo             = cldapi.NewProc("CfGetPlaceholderInfo")
	procCfHydratePlaceholder             = cldapi.NewProc("CfHydratePlaceholder")
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

// ShellNotifyCreated tells Explorer that a file or folder has appeared at path,
// so an open window lists it straight away. An item the provider creates with
// CfCreatePlaceholders is NOT picked up by a window that has just shown it
// being deleted (measured on the test VM: a folder put back after a refused
// delete stayed invisible until F5). Values from ShlObj_core.h, SDK 10.0.26100.
func ShellNotifyCreated(path string, isDir bool) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return
	}
	const (
		shcneCreate = 0x00000002
		shcneMkdir  = 0x00000008
		shcnfPathW  = 0x0005
	)
	ev := uintptr(shcneCreate)
	if isDir {
		ev = shcneMkdir
	}
	_, _, _ = procSHChangeNotify.Call(ev, shcnfPathW, uintptr(unsafe.Pointer(p)), 0)
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

// ShellSyncRootNamespaceCLSID returns the CLSID of the navigation-pane node
// Windows created for path's cloud sync root, or "" when path is not a
// registered root or Windows gave it no node. The registration records it as
// NamespaceCLSID on the root's SyncRootManager key (seen on Windows 11 26200;
// the node itself is an HKCU\Software\Classes\CLSID delegate folder pinned
// into the pane — the same shape as our own entry in internal/shellns).
func ShellSyncRootNamespaceCLSID(path string) string {
	id, err := shellRootID(path)
	if err != nil {
		return ""
	}
	for _, hive := range []registry.Key{registry.LOCAL_MACHINE, registry.CURRENT_USER} {
		if s := namespaceCLSIDAt(hive, syncRootManager+`\`+id); s != "" {
			return s
		}
	}
	return ""
}

func namespaceCLSIDAt(hive registry.Key, keyPath string) string {
	k, err := registry.OpenKey(hive, keyPath, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	s, _, err := k.GetStringValue("NamespaceCLSID")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(s)
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
//
// It is the FALLBACK hydration path: one call — and therefore one HTTP request
// — per transfer chunk. Install a HydrateStreamFunc instead (SetHydrateStream)
// to serve a whole request from a single reader.
type HydrateFunc func(identity []byte, offset, length int64) ([]byte, error)

// HydrateStreamFunc opens a reader over the byte range [offset, offset+length)
// of the file identified by identity — one reader for the whole of one
// FETCH_DATA request, however large, instead of HydrateFunc's request per
// chunk. The caller closes the reader.
//
// ctx is cancelled when the filter withdraws the request — measured cause: the
// request stopped making progress for 60s (a stream that keeps flowing is
// never cancelled, however long it runs). Abandon the download promptly: once
// withdrawn, the request is the filter's to re-issue, and nothing more should
// be transferred against its key.
type HydrateStreamFunc func(ctx context.Context, identity []byte, offset, length int64) (io.ReadCloser, error)

// ListFunc returns the children of a directory (rel is relative to the sync
// root, "" for the root, forward-slash separated) so they can be populated on
// demand.
type ListFunc func(rel string) []PlaceholderInfo

// RenameFunc receives a completed rename or move inside a sync root. Both
// arguments are absolute local paths (old, then new). Calls are delivered
// one at a time, in the order the filter reported them, on a goroutine owned
// by the mount; return promptly.
type RenameFunc func(oldPath, newPath string)

type provider struct {
	path    string
	hydrate HydrateFunc
	list    ListFunc
	mu      sync.Mutex
	rename  RenameFunc     // set after Mount via SetRenameHandler; nil = ignore
	renames chan [2]string // queued (old, new) pairs; created on first SetRenameHandler(fn != nil)
	worker  bool           // a delivery goroutine is running
	closed  bool           // stopRenames has run; enqueue/worker must stop

	// Hydration state, under its own lock: the FETCH_DATA and
	// CANCEL_FETCH_DATA callbacks run on threads the OS is waiting on and
	// must never queue behind rename delivery.
	fetchMu       sync.Mutex
	hydrateStream HydrateStreamFunc       // set after Mount via SetHydrateStream; nil = use hydrate
	inflight      map[int64]*fetchRequest // transferKey -> the download serving it
	fetchClosed   bool                    // cancelFetches has run; no new download may start
}

// fetchRequest identifies ONE download so the bookkeeping can tell two
// requests apart even when the filter reuses a transfer key value. Keying
// `inflight` by the key alone was enough to cancel the right download, but not
// to untrack it: a finishing goroutine's deferred delete would remove a
// successor that had just claimed the same key, leaving the newcomer
// uncancellable. Identity is the pointer, so the zero-field struct is fine.
type fetchRequest struct {
	cancel context.CancelFunc
}

var (
	providers            sync.Map // connKey int64 -> *provider
	fetchDataCallbackPtr = syscall.NewCallback(fetchDataCallback)
	cancelFetchDataPtr   = syscall.NewCallback(cancelFetchDataCallback)
	fetchPlaceholdersPtr = syscall.NewCallback(fetchPlaceholdersCallback)
	renameCompletionPtr  = syscall.NewCallback(renameCompletionCallback)
)

// CF_CALLBACK_REGISTRATION { CF_CALLBACK_TYPE Type; CF_CALLBACK Callback; }
type callbackRegistration struct {
	Type     int32
	_        int32
	Callback uintptr
}

const (
	cfCallbackTypeFetchData              = 0
	cfCallbackTypeCancelFetchData        = 2 // CF_CALLBACK_TYPE_CANCEL_FETCH_DATA
	cfCallbackTypeFetchPlaceholders      = 3
	cfCallbackTypeNotifyRenameCompletion = 12 // CF_CALLBACK_TYPE_NOTIFY_RENAME_COMPLETION
	cfCallbackTypeNone                   = -1
	cfConnectFlagNone                    = 0
	cfConnectFlagRequireProcessInfo      = 2 // CF_CONNECT_FLAG_REQUIRE_PROCESS_INFO
	cfConnectFlagRequireFullFilePath     = 4 // CF_CONNECT_FLAG_REQUIRE_FULL_FILE_PATH
	cfOperationTypeTransferData          = 0
	cfOperationTypeTransferPlaceholders  = 4
	// CF_CALLBACK_PARAMETERS.RenameCompletion.SourcePath: ParamSize@0, the
	// union at 8 (Flags@8), SourcePath@16 — verified by compiling cfapi.h
	// 10.0.26100 with MSVC 14.50 (2026-09-14). Pinned by
	// TestStructLayoutsMatchCfapiH.
	cpRenameSourcePath = 16
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
		// The withdrawal notice for a FETCH_DATA already in flight. Unlike
		// NOTIFY_RENAME it needs no acknowledgement of any kind — registering
		// it only ever stops work nobody is waiting for.
		{Type: cfCallbackTypeCancelFetchData, Callback: cancelFetchDataPtr},
		{Type: cfCallbackTypeFetchPlaceholders, Callback: fetchPlaceholdersPtr},
		// Completion only: the filter has already applied the rename and
		// expects no ACK. NOTIFY_RENAME (the pre-op, type 11) is deliberately
		// NOT registered — it must be acknowledged or every rename fails.
		{Type: cfCallbackTypeNotifyRenameCompletion, Callback: renameCompletionPtr},
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
	if pv, ok := providers.Load(connKey); ok {
		pv.(*provider).stopRenames()
		pv.(*provider).cancelFetches()
	}
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
	if pv, ok := providers.Load(connKey); ok {
		pv.(*provider).stopRenames()
		pv.(*provider).cancelFetches()
	}
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
	// UploadTime is the server's nc:upload_time for this version (0 = not
	// reported); with Size and ModTime it tells a metadata-only ETag bump
	// from a real edit (transport.ContentKey). Not stored in the placeholder.
	UploadTime int64
	FileID     string // server oc:fileid — stable across renames; used for down-sync rename detection
	// MountRoot marks the top of a share received from someone else or of a
	// mount: the one entry whose later disappearance from a listing means
	// "detached from this account", not "deleted" (Deck #557).
	MountRoot bool
	// Encrypted marks an end-to-end encrypted folder. Population never creates
	// one; only the delete guard's complete listing reports them, so a folder
	// holding one is never deleted on the strength of a listing that hid it.
	Encrypted bool
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
	// One long identity poisons its batch-mates (see longIdentityBytes): the
	// short ones go together, each long one goes alone.
	short, long := splitLongIdentities(items)
	if len(short) > 0 {
		if err := createPlaceholdersRaw(baseW, short); err != nil {
			return err
		}
	}
	for _, it := range long {
		if err := createPlaceholdersRaw(baseW, []PlaceholderInfo{it}); err != nil {
			return err
		}
	}
	return nil
}

// longIdentityBytes is the FileIdentity length from which a placeholder must
// be created in a batch of its own.
//
// Measured on Windows 11 10.0.26200 (2026-09-15/16, test VM and dev box, via
// the public API from a plain process): when CfCreatePlaceholders or
// CfExecute(TRANSFER_PLACEHOLDERS) is handed an array in which one entry's
// FileIdentity is ~133 bytes or longer, that entry is created correctly and
// the OTHER entries of the same call come out with corrupt cloud-file
// metadata — ERROR_CLOUD_FILE_METADATA_CORRUPT (363) on every open, even as
// the connected provider; undeletable; permanent. 132 bytes is fine, 136 is
// not, the name's length is irrelevant, and a long identity created alone is
// healthy. Our identity is the raw server path, so any directory holding an
// entry that deep stranded all its siblings on population and on reconcile's
// pull (GitHub #7's "corrupt metadata" placeholders). 128 leaves a margin
// under the measured edge; batch_windows_test.go pins both the fault and
// this defence.
const longIdentityBytes = 128

// splitLongIdentities partitions items into those safe to create together
// and those that must each be created alone. Order within each part is kept.
func splitLongIdentities(items []PlaceholderInfo) (short, long []PlaceholderInfo) {
	for _, it := range items {
		if len(it.Identity) >= longIdentityBytes {
			long = append(long, it)
		} else {
			short = append(short, it)
		}
	}
	return short, long
}

// createPlaceholdersRaw is one CfCreatePlaceholders call; callers split the
// batch first (see longIdentityBytes).
func createPlaceholdersRaw(baseW *uint16, items []PlaceholderInfo) error {
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
	// CF_CALLBACK_PARAMETERS (Cancel.FetchData) offsets. Cancel nests a second
	// union inside itself, so its Flags takes the outer union's slot at 8 and
	// the withdrawn range follows at 16/24 — the same places FETCH_DATA's
	// required range sits. Pinned by TestStructLayoutsMatchCfapiH.
	cpCancelFlags  = 8
	cpCancelOffset = 16
	cpCancelLength = 24
)

// cfTransferChunk is the MOST of a hydration request handed to the filter in
// one CfExecute(TRANSFER_DATA) — the piece size a link fast enough to fill it
// promptly will actually use. 4 MiB replaced 1 MiB when hydration stopped
// costing an HTTP request per piece: with one reader serving the whole
// request, a bigger piece is simply fewer syscalls (VM, v0.1.0.292: 64
// transfers for 256 MB).
const cfTransferChunk = 4 << 20

// cfTransferAlign is the sector alignment the filter requires of every
// transfer that does not finish the request: a mid-file piece not landing on a
// 4 KiB boundary is rejected. Only the piece completing the request may be any
// length.
const cfTransferAlign = 4096

// cfPieceFlushAfter bounds how long one piece may spend filling before
// whatever is in hand (its 4096-aligned prefix) is handed over anyway. The
// filter withdraws a request that goes ~60s without a TRANSFER_DATA, so
// waiting for a whole piece imposed a MINIMUM SUSTAINABLE RATE of
// 4 MiB / 60s ≈ 70 KiB/s — and below it hydration could never finish at all:
// cancelled before the first piece, re-requested at the same offset, cancelled
// again, until the opener's own ~180s timeout, discarding up to 4 MiB per
// cycle. Not hypothetical: the download cap in Settings (and the DownloadKBps
// policy) has no lower bound in the UI, so 68 KiB/s is a setting a user can
// type. The 1 MiB loop had the same shape with a ~17 KiB/s floor. Flushing on
// a timer instead drops the floor to cfTransferAlign / 60s ≈ 68 BYTES/s while
// leaving a fast link's full 4 MiB pieces untouched.
const cfPieceFlushAfter = 15 * time.Second

// cfReadStallLimit is how long a hydration stream may deliver nothing at all
// before the request is failed. The filter withdraws a request that goes ~60s
// without progress, but only when another process is waiting on it; Nimbo's
// own hydrations (keeping a pinned file on this device) are never withdrawn,
// so a link that stayed up but stopped sending held the read, the request
// and the pin for good. Longer than the filter's 60s on purpose, so that for
// every other request its withdrawal still comes first and nothing changes.
const cfReadStallLimit = 90 * time.Second

// testReadStallLimit overrides cfReadStallLimit when non-zero. Set only by
// tests.
var testReadStallLimit time.Duration

func readStallLimit() time.Duration {
	if testReadStallLimit > 0 {
		return testReadStallLimit
	}
	return cfReadStallLimit
}

// errReadStalled is the cause streamFetch cancels its stream with when the
// watchdog fires.
var errReadStalled = errors.New("the hydration stream delivered nothing for too long")

// execTransfer and execTransferFail are the CfExecute calls streamFetch
// makes, as variables so its tests can run without a sync root.
var (
	execTransfer     = cfTransfer
	execTransferFail = cfTransferFail
)

// testPieceFlushAfter overrides cfPieceFlushAfter when non-zero. Set ONLY by
// the slow-link test, which would otherwise have to run for a minute to
// observe a timed flush at all.
var testPieceFlushAfter time.Duration

func pieceFlushAfter() time.Duration {
	if testPieceFlushAfter > 0 {
		return testPieceFlushAfter
	}
	return cfPieceFlushAfter
}

// statusUnsuccessful is STATUS_UNSUCCESSFUL (ntstatus.h:2074), the completion
// status for a hydration request the provider could not satisfy. cfapi.h says
// nothing beyond "NTSTATUS CompletionStatus", and the documented generic
// failure is this one; STATUS_CLOUD_FILE_NETWORK_UNAVAILABLE (0xC000CF11)
// exists but claims a specific cause we usually do not know (the reader may
// have failed for any reason), so the generic code is used and the real error
// goes to the debug log instead.
const statusUnsuccessful = int32(-1073741823) // 0xC0000001

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
	p := pv.(*provider)
	p.fetchMu.Lock()
	stream := p.hydrateStream
	p.fetchMu.Unlock()

	identity := make([]byte, idLen) // copy — the OS buffer is only valid during the callback
	if idLen > 0 {
		copy(identity, unsafe.Slice((*byte)(unsafe.Pointer(idPtr)), idLen))
	}

	if stream != nil {
		ctx, cancel := context.WithCancel(context.Background())
		req, ok := p.trackFetch(transferKey, cancel)
		if !ok {
			// The mount is going away; the connection key is already dead, so
			// there is nothing to complete and nothing worth downloading.
			dbg("FETCH_DATA %q: the provider is shutting down, dropped", idStr)
			return 0
		}
		go func() {
			defer p.untrackFetch(transferKey, req)
			defer cancel()
			p.streamFetch(ctx, stream, connKey, transferKey, idStr, identity, reqOffset, reqLength)
		}()
		return 0
	}

	hyd := p.hydrate
	go func() {
		off := reqOffset
		remaining := reqLength
		for remaining > 0 {
			n := int64(cfTransferChunk)
			if n > remaining {
				n = remaining
			}
			data, err := hyd(identity, off, n)
			if err != nil || len(data) == 0 {
				// Complete the request as FAILED rather than walking away:
				// an abandoned request leaves the caller's open blocked
				// until the filter's own time-out (measured: 180s).
				dbg("FETCH_DATA %q: hydrate offset=%d len=%d: %v", idStr, off, n, err)
				cfTransferFail(connKey, transferKey, off, remaining)
				return
			}
			if !cfTransfer(connKey, transferKey, off, data) {
				cfTransferFail(connKey, transferKey, off, remaining)
				return
			}
			off += int64(len(data))
			remaining -= int64(len(data))
		}
	}()
	return 0
}

// streamFetch serves one whole FETCH_DATA request from a single reader,
// handing the bytes to the filter a piece at a time. A piece is up to
// cfTransferChunk bytes, but it is also flushed once cfPieceFlushAfter has
// passed with at least cfTransferAlign bytes in hand, so the filter keeps
// seeing progress on a link too slow to fill 4 MiB inside its ~60s stall
// timeout (see cfPieceFlushAfter — below ~70 KiB/s, waiting for full pieces
// meant hydration never completed at all). A fast link still fills every
// piece and transfers exactly as before (VM, v0.1.0.292: 1 GET and 64
// transfers in 10s for 256 MB, where the per-chunk request loop needed 256
// GETs and 60s).
//
// Alignment holds either way: a flushed piece hands over only its
// 4096-aligned prefix and CARRIES THE TAIL into the next piece, so every
// transfer but the request's last lands on a sector boundary.
//
// Whatever happens, the request is COMPLETED: fully transferred, or failed for
// the part that could not be served. Returning without doing either is the
// defect this replaces — the caller's open then hangs on the filter's time-out.
// The one exception is a request the filter WITHDREW (ctx cancelled from
// outside): its transfer key is dead and there is nothing left to complete.
//
// A stream that delivers nothing for readStallLimit, opening included, is
// cancelled here and the request failed (see cfReadStallLimit).
func (p *provider) streamFetch(ctx context.Context, stream HydrateStreamFunc, connKey, transferKey int64, idStr string, identity []byte, reqOffset, reqLength int64) {
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	limit := readStallLimit()
	watchdog := time.AfterFunc(limit, func() { stop(errReadStalled) })
	defer watchdog.Stop()
	// stalled tells the watchdog's cancel apart from a withdrawal: only the
	// first leaves a live transfer key that must still be answered.
	stalled := func() bool { return errors.Is(context.Cause(ctx), errReadStalled) }

	rc, err := stream(ctx, identity, reqOffset, reqLength)
	if err != nil {
		if stalled() {
			dbg("FETCH_DATA %q: the stream did not open within %v, failing the request", idStr, limit)
			execTransferFail(connKey, transferKey, reqOffset, reqLength)
			return
		}
		if ctx.Err() != nil {
			// The request was withdrawn while the stream was being opened —
			// err is that cancellation, not a download failure, and the
			// transfer key is gone. Completing it would only log a rejection.
			dbg("FETCH_DATA %q: withdrawn while opening the stream", idStr)
			return
		}
		dbg("FETCH_DATA %q: open stream: %v", idStr, err)
		execTransferFail(connKey, transferKey, reqOffset, reqLength)
		return
	}
	if rc == nil {
		// A broken HydrateStreamFunc, not a download failure — but the
		// request still has to be answered, and a nil-interface Close would
		// panic on a callback goroutine and take the process with it.
		dbg("FETCH_DATA %q: the stream func returned no reader and no error", idStr)
		execTransferFail(connKey, transferKey, reqOffset, reqLength)
		return
	}
	defer rc.Close()

	buf := make([]byte, cfTransferChunk)
	flushAfter := pieceFlushAfter()
	off, remaining := reqOffset, reqLength
	held := 0 // bytes at the front of buf: a previous piece's unaligned tail
	for remaining > 0 {
		if ctx.Err() != nil && !stalled() {
			// Withdrawn: the transfer key is dead, so there is nothing to
			// complete — the filter has already failed the caller's I/O.
			dbg("FETCH_DATA %q: cancelled with %d bytes left", idStr, remaining)
			return
		}
		want := len(buf)
		if int64(want) > remaining {
			want = int(remaining)
		}

		// Fill the piece, but never past flushAfter once there is an aligned
		// prefix to hand over. Each Read returns whatever has arrived, so on a
		// slow link this loop wakes often and the deadline is honoured
		// closely; on a fast one it simply fills.
		n, started := held, time.Now()
		var rerr error
		for n < want {
			rn, err := rc.Read(buf[n:want])
			if rn > 0 {
				watchdog.Reset(limit) // the limit is on silence, not on the whole download
			}
			n += rn
			if n >= want {
				break // piece full; any error travels with the next read
			}
			if err != nil {
				rerr = err
				break
			}
			if n >= cfTransferAlign && time.Since(started) >= flushAfter {
				dbg("FETCH_DATA %q: slow link, flushing %d of %d bytes after %v", idStr, n, want, time.Since(started).Round(time.Millisecond))
				break
			}
		}

		if rerr != nil {
			// The stream could not deliver the rest of the range, so the open
			// is going to fail either way and the n bytes in hand are
			// deliberately DROPPED: they would buy the caller nothing, and a
			// partial piece is the one shape that can land off a sector
			// boundary.
			if stalled() {
				dbg("FETCH_DATA %q: nothing received for %v with %d bytes left (%d in hand, dropped), failing the request", idStr, limit, remaining, n)
				execTransferFail(connKey, transferKey, off, remaining)
			} else if ctx.Err() == nil {
				dbg("FETCH_DATA %q: stream ended %d bytes short (%d in hand, dropped): %v", idStr, remaining, n, rerr)
				execTransferFail(connKey, transferKey, off, remaining)
			} else {
				dbg("FETCH_DATA %q: cancelled with %d bytes left", idStr, remaining)
			}
			return
		}
		// Re-check: reading is where a download waits, so this is the one
		// place a cancel can land mid-piece. Without it, a withdrawn request
		// still pushes a piece at a dead transfer key. (A stall cannot land
		// here with a full piece in hand: the read that filled it reset the
		// watchdog, so the piece is still worth sending and the next read
		// meets the stall.)
		if ctx.Err() != nil && !stalled() {
			dbg("FETCH_DATA %q: cancelled with %d bytes left", idStr, remaining)
			return
		}

		// Only the transfer that completes the request may be unaligned; a
		// piece flushed early keeps its ragged tail for next time.
		send := n
		if int64(n) < remaining {
			send -= n % cfTransferAlign
		}
		if send <= 0 {
			// Unreachable: the fill loop only stops short of a full piece
			// once cfTransferAlign bytes are in hand. Fail the request rather
			// than spin on a piece that cannot grow or hand CfExecute an
			// empty buffer (which would panic on a callback goroutine).
			dbg("FETCH_DATA %q: nothing sendable from %d bytes with %d left", idStr, n, remaining)
			execTransferFail(connKey, transferKey, off, remaining)
			return
		}
		if !execTransfer(connKey, transferKey, off, buf[:send]) {
			// The filter refused the piece; fail the rest from here so it
			// never waits for a gap we are not going to fill.
			execTransferFail(connKey, transferKey, off, remaining)
			return
		}
		off += int64(send)
		remaining -= int64(send)
		held = copy(buf, buf[send:n])
	}
}

// cancelFetchDataCallback fires when the filter withdraws a hydration request
// — measured cause (see the live test): the request went 60s without
// progress, reported as CF_CALLBACK_CANCEL_FLAG_IO_TIMEOUT. Stop the download.
// Nothing is executed in response: a withdrawn request needs no completion,
// and the filter re-issues the unserved remainder under a NEW transfer key
// when anyone is still waiting for it.
func cancelFetchDataCallback(info, params uintptr) uintptr {
	connKey := *(*int64)(unsafe.Pointer(info + ciConnectionKey))
	transferKey := *(*int64)(unsafe.Pointer(info + ciTransferKey))
	flags := *(*uint32)(unsafe.Pointer(params + cpCancelFlags))
	offset := *(*int64)(unsafe.Pointer(params + cpCancelOffset))
	length := *(*int64)(unsafe.Pointer(params + cpCancelLength))
	pv, ok := providers.Load(connKey)
	if !ok {
		dbg("CANCEL_FETCH_DATA: no provider for conn=%d", connKey)
		return 0
	}
	stopped := pv.(*provider).cancelFetch(transferKey)
	dbg("CANCEL_FETCH_DATA flags=0x%x offset=%d length=%d stopped=%v", flags, offset, length, stopped)
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

// CF_CALLBACK_PARAMETERS as the NOTIFY_RENAME_COMPLETION callback sees it — a
// prefix of the real 64-byte union, enough to pin SourcePath's offset.
type callbackParamsRenameCompletion struct {
	ParamSize  uint32
	_          uint32 // union alignment — see opParamsTransfer
	Flags      uint32
	_          uint32
	SourcePath uintptr
}

// CF_CALLBACK_PARAMETERS as the CANCEL_FETCH_DATA callback sees it. Cancel's
// own nested union holds LARGE_INTEGERs, so there is a second four bytes of
// padding after Flags. Exists only to pin cpCancel* in the layout test — the
// callback reads the fields by offset like its neighbours do.
type callbackParamsCancelFetchData struct {
	ParamSize  uint32
	_          uint32 // outer union alignment — see opParamsTransfer
	Flags      uint32
	_          uint32 // Cancel's nested union is 8-aligned
	FileOffset int64
	Length     int64
}

// cfTransfer hands one piece of a hydration request to the filter. Reports
// whether the filter accepted it: a refusal (most often a dead transfer key
// after a cancel) leaves a gap the caller must account for rather than keep
// feeding bytes into.
func cfTransfer(connKey, transferKey, offset int64, data []byte) bool {
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
		return false
	}
	dbg("CfExecute(TRANSFER_DATA) offset=%d len=%d -> ok", offset, len(data))
	return true
}

// cfTransferFail completes a hydration request the provider could not satisfy:
// a TRANSFER_DATA carrying no data, a failure CompletionStatus, and the length
// left unserved at offset. That is what turns the caller's blocked open into a
// prompt error instead of a three-minute wait on the filter's own time-out
// (measured: 180s before this existed).
//
// Buffer is deliberately nil — a failed completion carries no bytes, and
// cfapi.h only annotates Buffer with the size of Length (it never says a
// failure needs one). Verified live: the filter accepts the nil buffer and
// fails the open at once. Also verified end-to-end on the VM (v0.1.0.292):
// with the server blocked, opening an online-only file fails in 7s and the
// placeholder is left online-only, rather than hanging for three minutes.
func cfTransferFail(connKey, transferKey, offset, length int64) {
	oi := operationInfo{Type: cfOperationTypeTransferData, ConnectionKey: connKey, TransferKey: transferKey}
	oi.StructSize = uint32(unsafe.Sizeof(oi))
	op := opParamsTransfer{
		CompletionStatus: statusUnsuccessful,
		Offset:           offset,
		Length:           length,
	}
	op.ParamSize = uint32(unsafe.Sizeof(op))
	hr, _, _ := procCfExecute.Call(uintptr(unsafe.Pointer(&oi)), uintptr(unsafe.Pointer(&op)))
	if int32(hr) < 0 {
		dbg("CfExecute(TRANSFER_DATA fail) offset=%d len=%d -> 0x%08x", offset, length, uint32(hr))
	} else {
		dbg("CfExecute(TRANSFER_DATA fail) offset=%d len=%d -> ok", offset, length)
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
//
// Tries the anchored match (normalizedRel) first so the two can't diverge;
// falls back to the old unanchored substring match for whatever it used to
// accept that normalizedRel's stricter boundary check now rejects. This path
// only ever feeds FETCH_PLACEHOLDERS population, not a security-relevant
// decision, so the looser fallback is harmless.
func relFromNormalized(syncRoot, normalized string) string {
	if rel, ok := normalizedRel(syncRoot, normalized); ok {
		return rel
	}
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
		dir := filepath.Join(p.path, filepath.FromSlash(rel))
		ok := cfTransferPlaceholders(connKey, transferKey, dir, items)
		// Deliberately NOT marked in-sync here. Marking the directory while the
		// enumeration that triggered this fetch is still in flight makes that
		// enumeration return EMPTY (measured on the live driver — the first
		// attempt at Deck #576 did exactly this and TestStateBitsAcrossLifecycle
		// caught it). It is marked a minute later instead; SweepDirsInSync
		// still covers anything that timer misses (a disconnect in between).
		// The root is never marked, as in the sweep.
		if ok && items != nil && rel != "" {
			settleLater(connKey, dir)
		}
	}()
	if experimentSyncFetch {
		<-syncDone // EXPERIMENT: complete before the callback returns
	}
	return 0
}

// SetHydrateStream installs (nil clears) the streaming hydration function for
// the mount identified by connKey. While one is installed it serves every
// FETCH_DATA request — one reader per request — and the byte-slice HydrateFunc
// passed to Mount is not used at all. Set it right after Mount, before
// anything can open a placeholder.
func SetHydrateStream(connKey int64, f HydrateStreamFunc) {
	pv, ok := providers.Load(connKey)
	if !ok {
		return
	}
	p := pv.(*provider)
	p.fetchMu.Lock()
	p.hydrateStream = f
	p.fetchMu.Unlock()
}

// trackFetch registers an in-flight download so CANCEL_FETCH_DATA (and
// teardown) can stop it, returning the handle its goroutine must pass back to
// untrackFetch. It reports false — having already cancelled — when the
// provider is being torn down: a FETCH_DATA that loaded the provider just
// before Unmount or Disconnect must not start a download that outlives the
// connection, nor re-create the map cancelFetches just cleared.
func (p *provider) trackFetch(transferKey int64, cancel context.CancelFunc) (*fetchRequest, bool) {
	p.fetchMu.Lock()
	if p.fetchClosed {
		p.fetchMu.Unlock()
		cancel()
		return nil, false
	}
	req := &fetchRequest{cancel: cancel}
	if p.inflight == nil {
		p.inflight = make(map[int64]*fetchRequest)
	}
	p.inflight[transferKey] = req
	p.fetchMu.Unlock()
	return req, true
}

// untrackFetch forgets a finished download — the map must not be allowed to
// grow forever, since every hydration in the session passes through it. The
// delete is conditional on the entry still being OURS: if the filter has
// meanwhile reused the key value for a new request, that successor's entry
// must survive (see fetchRequest).
func (p *provider) untrackFetch(transferKey int64, req *fetchRequest) {
	p.fetchMu.Lock()
	defer p.fetchMu.Unlock()
	if p.inflight[transferKey] == req {
		delete(p.inflight, transferKey)
	}
}

// cancelFetch stops the download serving transferKey, if one is still running.
// Reports whether there was one (a cancel for an already-finished request is
// normal and not worth a complaint).
func (p *provider) cancelFetch(transferKey int64) bool {
	p.fetchMu.Lock()
	req, ok := p.inflight[transferKey]
	p.fetchMu.Unlock()
	if ok {
		req.cancel()
	}
	return ok
}

// cancelFetches stops every in-flight download and refuses any later one.
// Unmount and Disconnect call it: past that point the connection key is dead,
// so a download still running can only burn bandwidth and log rejected
// transfers. Final, like stopRenames — a provider is never reconnected, Mount
// builds a new one.
func (p *provider) cancelFetches() {
	p.fetchMu.Lock()
	inflight := p.inflight
	p.inflight = nil
	p.fetchClosed = true
	p.fetchMu.Unlock()
	for _, req := range inflight {
		req.cancel()
	}
}

// SetRenameHandler installs (nil clears) the function told about completed
// renames and moves under the mount identified by connKey. The first call
// with a non-nil fn starts one delivery goroutine for the provider (see
// runRenameWorker); a later call just swaps the handler the running
// goroutine calls — it never restarts anything.
func SetRenameHandler(connKey int64, fn RenameFunc) {
	pv, ok := providers.Load(connKey)
	if !ok {
		return
	}
	p := pv.(*provider)
	p.mu.Lock()
	p.rename = fn
	if fn != nil && !p.worker && !p.closed {
		if p.renames == nil {
			p.renames = make(chan [2]string, 256)
		}
		p.worker = true
		go p.runRenameWorker()
	}
	p.mu.Unlock()
}

// runRenameWorker delivers queued renames to the CURRENT handler one at a
// time, in the order the filter reported them, until stopRenames closes the
// queue. Reading p.rename under mu on every iteration (rather than once at
// startup) is what lets a later SetRenameHandler swap the handler without
// restarting this goroutine. One worker per provider bounds concurrency to
// exactly one in-flight call, so a same-item double-rename or an Explorer
// multi-select move can't be delivered out of order or spawn unbounded
// goroutines — both measured problems with the previous "go fn(...)" per
// event. Found in review, 2026-09-14.
func (p *provider) runRenameWorker() {
	for pair := range p.renames {
		p.mu.Lock()
		fn := p.rename
		p.mu.Unlock()
		if fn != nil {
			fn(pair[0], pair[1])
		}
	}
}

// enqueueRename hands a completed rename to the provider's delivery queue
// with a non-blocking send: the filter's callback thread must never block,
// so a full queue drops the event (logging it) rather than waiting, and a
// provider with no handler installed yet (or already stopped) drops it
// silently. Factored out of renameCompletionCallback so a unit test can
// drive it directly, without the live driver.
func (p *provider) enqueueRename(old, new string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.renames == nil {
		return
	}
	select {
	case p.renames <- [2]string{old, new}:
	default:
		dbg("NOTIFY_RENAME_COMPLETION: queue full, dropped %q -> %q", old, new)
	}
}

// stopRenames shuts down rename delivery for good: Unmount and Disconnect
// both call this (before dropping the provider from the registry) so the
// worker goroutine, if any, exits instead of leaking past the connection's
// lifetime.
func (p *provider) stopRenames() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	if p.renames != nil {
		close(p.renames)
	}
}

// normalizedRel maps a callback's NormalizedPath (volume-relative, no drive
// letter, e.g. `\Users\Adam\NimboRoot\a\x`) — or a full path — to the
// sync-root-relative forward-slash path. ok is false for a path outside the
// root, including a sibling whose name merely starts with the root's.
//
// The match is ANCHORED: root and input both have any leading "X:" drive
// stripped, then the input must have the root as a genuine PREFIX
// (strings.HasPrefix), not merely contain it anywhere. An unanchored
// strings.Index (the original implementation) let root `D:\Nimbo` match
// input `\Users\Adam\Nimbo\x.bin` (the root's name buried mid-path) or
// `D:\Backup\Nimbo\x.bin` (a different tree that happens to contain a `Nimbo`
// component) — both wrongly accepted as "inside the root". Found in review,
// 2026-09-14.
func normalizedRel(root, normalized string) (string, bool) {
	r := root
	if len(r) >= 2 && r[1] == ':' {
		r = r[2:]
	}
	p := normalized
	if len(p) >= 2 && p[1] == ':' {
		p = p[2:]
	}
	lp, lr := strings.ToLower(p), strings.ToLower(r)
	if !strings.HasPrefix(lp, lr) {
		return "", false
	}
	rest := p[len(r):]
	if rest != "" && rest[0] != '\\' {
		return "", false
	}
	return strings.Trim(strings.ReplaceAll(rest, `\`, "/"), "/"), true
}

// renameCompletionCallback fires after the filter has applied a rename or a
// move of any item inside the sync root — including a move between
// directories, which ReadDirectoryChangesW reports only as REMOVED + ADDED
// (issue #7: that pair became a server DELETE of the source). The work runs
// on a goroutine so the filter's thread returns at once. Moves that cross the
// root boundary are ignored here; the watcher's own events cover them.
//
// Two measured facts (2026-09-14, Windows 10.0.26200), neither documented in
// cfapi.h: the filter does NOT report renames made by the connected
// provider's own process (only renames from another process arrive here),
// and it does NOT report renames of plain (non-placeholder) files or
// directories — only placeholders. Both gaps are already covered elsewhere:
// Nimbo's own renames are down-sync pulls the watcher already suppresses,
// and the watcher's REMOVED+ADDED pairing by placeholder identity covers
// plain items.
func renameCompletionCallback(info, params uintptr) uintptr {
	connKey := *(*int64)(unsafe.Pointer(info + ciConnectionKey))
	newNorm := readUTF16(*(*uintptr)(unsafe.Pointer(info + ciNormalizedPath)))
	oldNorm := readUTF16(*(*uintptr)(unsafe.Pointer(params + cpRenameSourcePath)))
	dbg("NOTIFY_RENAME_COMPLETION conn=%d %q -> %q", connKey, oldNorm, newNorm)
	pv, ok := providers.Load(connKey)
	if !ok {
		return 0
	}
	p := pv.(*provider)
	oldRel, okOld := normalizedRel(p.path, oldNorm)
	newRel, okNew := normalizedRel(p.path, newNorm)
	if !okOld || !okNew || oldRel == "" || newRel == "" {
		dbg("NOTIFY_RENAME_COMPLETION: outside the root, ignored")
		return 0
	}
	oldAbs := filepath.Join(p.path, filepath.FromSlash(oldRel))
	newAbs := filepath.Join(p.path, filepath.FromSlash(newRel))
	p.enqueueRename(oldAbs, newAbs)
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

// cfTransferPlaceholders delivers items and reports whether the whole delivery
// took. dir is the local directory being populated.
//
// The return value is informational: the sole caller
// (fetchPlaceholdersCallback) discards it, because the directory is
// deliberately NOT marked in-sync there, so a failure shows up as the debug
// lines below and heals through reconcile's pull rather than through the
// caller. Keep that in mind before reading a "false" as if anything acted on
// it.
//
// Two things are taken out of the batch before it is handed to the filter:
//   - entries that already exist locally. The shell re-asks the ROOT to
//     populate after every (re)connect, and a directory's listing always
//     includes what is already there; the kernel would answer ALREADY_EXISTS
//     per entry, but an existing entry with a long identity poisons the
//     batch just like a new one (measured), and the reporter's stranded
//     files were root additions made while Nimbo was closed, created by
//     exactly such a re-fetch.
//   - entries with a long identity (longIdentityBytes). They are created
//     alone through CfCreatePlaceholders, which is allowed while the
//     enumeration is pending, and the transfer then carries the rest.
func cfTransferPlaceholders(connKey, transferKey int64, dir string, items []PlaceholderInfo) bool {
	longOK := true
	if items != nil {
		fresh := items[:0:0]
		for _, it := range items {
			if _, serr := os.Lstat(filepath.Join(dir, it.Name)); serr == nil || !os.IsNotExist(serr) {
				continue // already there (or unreadable): not ours to create
			}
			fresh = append(fresh, it)
		}
		var long []PlaceholderInfo
		items, long = splitLongIdentities(fresh)
		if items == nil {
			// Everything was filtered out or created alone: the listing still
			// SUCCEEDED, and nil below means "listing failed, do not mark the
			// directory populated" — the shell would then re-ask forever.
			items = []PlaceholderInfo{}
		}
		for _, it := range long {
			if cerr := CreatePlaceholders(dir, []PlaceholderInfo{it}); cerr != nil {
				dbg("TRANSFER_PLACEHOLDERS: long-identity entry %q created alone -> %v", it.Name, cerr)
				longOK = false
			}
		}
	}
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
	// A failed entry leaves the directory partially populated with the shell
	// re-requesting it, and a directory in that state must NOT be marked
	// in-sync (an in-sync directory is never asked to populate — see
	// TestInSyncDirStillPopulates). Nothing does that on this path today, so
	// say the long-alone failure out loud: it is the one outcome the
	// per-entry results above cannot show, since such an entry was never in
	// the array.
	if !longOK {
		dbg("TRANSFER_PLACEHOLDERS: transfer incomplete: a long-identity entry failed; the shell may re-ask")
	}
	return int32(hr) >= 0 && entriesOK && longOK
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
	// Placeholder reports whether the item is a cloud placeholder at all (a
	// plain file or directory is not).
	Placeholder bool
	// InSync is the RAW in-sync bit, which NeedsUpload deliberately no longer
	// echoes: the filter clears it for a rename as readily as for an edit, so
	// a clean placeholder that was merely moved reads InSync == false with
	// NeedsUpload == false. Callers that need to tell that state apart — to
	// restore the bit rather than upload — look here.
	InSync bool
}

// Inspect classifies path for write-back: a placeholder holding unsynced local
// content (the user edited a hydrated file) or a brand-new non-placeholder
// file/dir needs uploading; a clean placeholder — including one we just
// hydrated, and one another process just renamed — does not.
//
// The in-sync bit is a PRE-FILTER here, not the verdict. Measured live
// 2026-09-15 (Windows 10.0.26200): the cloud filter clears a placeholder's
// in-sync bit on any rename or move by another process, content untouched, and
// a cross-directory move also delivers FILE_ACTION_MODIFIED for the
// destination — so reading the bit as "dirty" made every move of an
// online-only stub schedule an upload of a file holding no local data, which
// parked the just-moved server copy as a conflicted copy. For a file that is
// not in sync, the real question is whether it holds local bytes the server
// has not got, and only unsyncedLocalData can answer that. The extra metadata
// open costs nothing in steady state: an in-sync file never reaches it.
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
	ch := Change{IsDir: isDir, Placeholder: isPlaceholder, InSync: inSync}
	switch {
	case isDir:
		// Our directory placeholders are deliberately NOT in-sync (that's how
		// lazy FETCH_PLACEHOLDERS population is triggered), so "not in sync" must
		// NOT be read as a change. Only a non-placeholder dir is a folder the
		// user just created and needs MKCOL.
		ch.NeedsUpload = !isPlaceholder
	case !isPlaceholder:
		ch.NeedsUpload = true // never uploaded: all of it is local-only content
	case inSync:
		ch.NeedsUpload = false
	default:
		// Not in sync: an edit, or just a rename. Ask the data.
		mod, merr := unsyncedLocalData(path)
		// Unreadable (a sharing violation, say) — keep the old, conservative
		// answer. A needless upload retries harmlessly; a missed one is an
		// edit the server never hears about.
		ch.NeedsUpload = merr != nil || mod
	}
	return ch, nil
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
	// openForCloud's attrs-only handle is enough for both branches below,
	// CfSetInSyncState on an existing placeholder and CfConvertToPlaceholder
	// on a plain file — verified live 2026-09-14 even when the plain file
	// carries real local content (the convert leaves the bytes untouched).
	h, err := openForCloud(path)
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
		if info, ierr := d.Info(); ierr == nil && info.ModTime().After(cutoff) {
			return nil // recently active; let it settle until the next sweep
		}
		if settleDir(path) {
			marked++
		}
		return nil
	})
	return marked
}

// settleDir gives one populated, not-yet-in-sync directory placeholder its
// in-sync state and redraws it. Returns true when it marked it. Everything
// that is not exactly that — a plain directory, one already in sync, one whose
// population never completed (PARTIAL: marking it would freeze it empty) — is
// left alone. Timing is the caller's job: see SweepDirsInSync and settleLater.
func settleDir(path string) bool {
	attrs, tag, ferr := findAttrTag(path)
	if ferr != nil || attrs&fileAttrDirectory == 0 {
		return false
	}
	r1, _, _ := procCfGetPlaceholderStateFromAttrTag.Call(uintptr(attrs), uintptr(tag))
	state := uint32(r1)
	if state == cfPlaceholderStateInvalid {
		return false
	}
	if state&cfPlaceholderStatePlaceholder == 0 || state&cfPlaceholderStateInSync != 0 {
		return false // not ours, or already correct
	}
	if state&cfPlaceholderStatePartial != 0 {
		return false // not (successfully) populated yet — marking would freeze it empty
	}
	if merr := MarkInSync(path, nil); merr != nil {
		dbg("settleDir %q: %v", path, merr)
		return false
	}
	ShellNotifyUpdated(path) // redraw the folder glyph without a manual refresh
	return true
}

// dirSettleDelay is how long after the shell populates a directory we give it
// its in-sync state. Marking it inside the transfer poisons the enumeration
// that asked for it (see fetchPlaceholdersCallback), and that enumeration is
// done in well under a second once the transfer lands; a minute leaves a wide
// margin. Waiting for SweepDirsInSync instead left every folder the user
// opened wearing the "sync pending" arrows for up to six hours (GitHub #17).
// A var so a test need not sleep through it.
var dirSettleDelay = time.Minute

// settleLater marks dir in sync dirSettleDelay from now, if the provider that
// populated it is still connected then. A directory the shell re-asks about
// only gets another timer, and settleDir is a no-op on one already marked.
func settleLater(connKey int64, dir string) {
	time.AfterFunc(dirSettleDelay, func() {
		if _, ok := providers.Load(connKey); !ok {
			return // disconnected: the next mount's SweepDirsInSync catches it
		}
		settleDir(dir)
	})
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
// WantsFreeUp reports whether path is a file Explorer has asked to free up
// (UNPINNED) that still holds its data — the state SettlePin acts on. An
// attributes-only query: it never opens the file, so it cannot hydrate it.
func WantsFreeUp(path string) bool {
	attrs, _, err := findAttrTag(path)
	if err != nil {
		return false
	}
	return attrs&fileAttrDirectory == 0 && attrs&fileAttrUnpinned != 0 && attrs&0x400000 == 0
}

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
	return updateIdentity(path, identity, cfUpdateFlagMarkInSync)
}

// UpdateIdentityKeepState rewrites the identity and leaves the placeholder's
// in-sync state exactly as it was. For a file with an edit still waiting to
// upload, MARK_IN_SYNC is a lie the rest of the system then acts on: the
// write-back gate reads it as "nothing to send" and the next refresh
// dehydrates the edit away. Renaming such a file has to repoint it without
// touching that bit.
func UpdateIdentityKeepState(path string, identity []byte) error {
	return updateIdentity(path, identity, 0 /* CF_UPDATE_FLAG_NONE */)
}

func updateIdentity(path string, identity []byte, flags uintptr) error {
	h, err := openForCloud(path)
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
		flags,
		uintptr(unsafe.Pointer(&usn)),
		0, // Overlapped
	)
	runtime.KeepAlive(identity)
	if int32(hr) < 0 {
		return fmt.Errorf("CfUpdatePlaceholder: 0x%08x", uint32(hr))
	}
	return nil
}

// CF_PLACEHOLDER_STANDARD_INFO (cfapi.h 10.0.26100). Offsets verified by
// compiling the header with MSVC 14.50 (2026-09-14): PinState@32,
// InSyncState@36, FileId@40, SyncRootFileId@48, FileIdentityLength@56,
// FileIdentity@60, sizeof 64. The identity bytes continue past the struct in
// the same buffer.
type placeholderStandardInfo struct {
	OnDiskDataSize     int64
	ValidatedDataSize  int64
	ModifiedDataSize   int64
	PropertiesSize     int64
	PinState           uint32
	InSyncState        uint32
	FileId             int64
	SyncRootFileId     int64
	FileIdentityLength uint32
	FileIdentity       [1]byte
}

// ErrNotPlaceholder reports that a path is a plain file or directory, not a
// cloud placeholder (HRESULT_FROM_WIN32(ERROR_NOT_A_CLOUD_FILE)).
var ErrNotPlaceholder = errors.New("not a cloud placeholder")

// ErrIsDirectory reports that a call refused a DIRECTORY placeholder. Only
// SetInSync returns it: the filter couples a directory's in-sync state to its
// population, so a directory marked in-sync enumerates EMPTY forever
// (measured live — TestInSyncDirStillPopulates), which is why ours are
// created not in sync in the first place.
var ErrIsDirectory = errors.New("cloud placeholder is a directory")

const (
	cfPlaceholderInfoStandard = 1          // CF_PLACEHOLDER_INFO_STANDARD
	hrNotACloudFile           = 0x80070178 // ERROR_NOT_A_CLOUD_FILE (376)
)

// standardInfo reads CF_PLACEHOLDER_STANDARD_INFO for path into a buffer big
// enough to hold the identity that follows the fixed header (the platform caps
// it at 4 KB). ErrNotPlaceholder for a plain file or directory.
func standardInfo(path string) ([]byte, error) {
	h, err := openForCloud(path)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(h)
	buf := make([]byte, unsafe.Sizeof(placeholderStandardInfo{})+4096)
	var ret uint32
	hr, _, _ := procCfGetPlaceholderInfo.Call(
		uintptr(h),
		uintptr(cfPlaceholderInfoStandard),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
		uintptr(unsafe.Pointer(&ret)),
	)
	runtime.KeepAlive(buf)
	if uint32(hr) == hrNotACloudFile {
		return nil, ErrNotPlaceholder
	}
	if int32(hr) < 0 {
		return nil, fmt.Errorf("CfGetPlaceholderInfo: 0x%08x", uint32(hr))
	}
	return buf, nil
}

// PlaceholderModified reports whether a file holds LOCAL content the server
// has not got. A plain (non-placeholder) file counts as modified: it is
// content that was never uploaded.
//
// This, NOT the in-sync bit, is what "dirty" has to mean around a rename.
// Measured live 2026-09-15 on Windows 10.0.26200
// (TestRenameClearsInSyncButNotModifiedData): the cloud filter CLEARS a
// placeholder's in-sync bit whenever another process renames or moves it,
// content untouched — an online-only stub moved between folders went
// state 0x39 -> 0x31 (InSyncState 1 -> 0) with ModifiedDataSize still 0, so
// Inspect reported NeedsUpload for a file with no local data at all. A
// hydrated file written locally went the same way on the bit but with
// ModifiedDataSize 4096 (the whole file), and kept that across a move. So the
// in-sync bit says "renamed or edited"; the data says "edited".
// Treating the first as the second made every move of a clean stub schedule
// an upload, which parked the just-moved server copy as a conflicted copy.
func PlaceholderModified(path string) (bool, error) {
	return unsyncedLocalData(path)
}

// unsyncedLocalData is the one definition of "this file holds bytes the server
// has not got", shared by Inspect and PlaceholderModified.
//
// ModifiedDataSize answers it for every case but one, measured live
// 2026-09-15 on Windows 10.0.26200 (all sizes in bytes, of a 4096-byte file):
//
//	hydrated, clean               OnDisk 4096 Valid 4096 Modified 0    InSync 1
//	hydrated, 10 bytes written    OnDisk 4096 Valid 0    Modified 4096 InSync 0
//	hydrated, truncated to 100    OnDisk 100  Valid 0    Modified 100  InSync 0
//	hydrated, O_TRUNC + rewrite   OnDisk 11   Valid 0    Modified 11   InSync 0
//	hydrated, TRUNCATED TO ZERO   OnDisk 0    Valid 0    Modified 0    InSync 0  <-- blind spot
//	hydrated, clean, then moved   OnDisk 4096 Valid 4096 Modified 0    InSync 0
//	online-only stub, then moved  OnDisk 0    Valid 0    Modified 0    InSync 0
//	after MarkInSync (either one) Modified back to 0
//
// Truncating a hydrated file to exactly ZERO leaves ModifiedDataSize 0 — the
// value was re-read at 200ms, 1s and 3s and after a subsequent move, and never
// moved off 0. A file in that state is indistinguishable, from the placeholder
// info alone, from an empty in-sync placeholder whose bit a rename cleared. So
// the tie is broken in the direction that cannot lose data: an EMPTY file whose
// data is fully local (no RECALL_ON_DATA_ACCESS) and whose in-sync bit is clear
// counts as unsynced content. The cost when the guess is wrong is a redundant
// upload of a zero-byte file; the cost of guessing the other way is a
// truncation silently discarded, and then undone by the next dehydrate.
//
// Online-only stubs are untouched by that tie-break (they keep
// RECALL_ON_DATA_ACCESS), which is what makes it safe: the move-of-a-stub case
// this whole predicate exists for still reads clean.
func unsyncedLocalData(path string) (bool, error) {
	buf, err := standardInfo(path)
	if errors.Is(err, ErrNotPlaceholder) {
		return true, nil // never uploaded: all of it is local-only content
	}
	if err != nil {
		return false, err
	}
	info := (*placeholderStandardInfo)(unsafe.Pointer(&buf[0]))
	if info.ModifiedDataSize > 0 {
		return true, nil
	}
	if info.InSyncState != 0 || info.OnDiskDataSize != 0 {
		return false, nil
	}
	// Empty on disk: either a truncated-to-zero file (data present, nothing in
	// it) or a stub (no data present at all). Only the attributes tell them
	// apart. A directory is never either.
	attrs, _, aerr := findAttrTag(path)
	if aerr != nil || attrs&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return false, nil
	}
	return attrs&fileAttrRecallOnDataAccess == 0, nil
}

// SetInSync restores a placeholder's in-sync state and nothing else — no
// identity rewrite, no convert, no data access. It is the repair for a bit the
// cloud filter cleared on a rename or move that turned out to carry no local
// change: without it the item (and every ancestor folder) wears Explorer's
// "sync pending" arrows forever, because nothing else will ever look at it
// again. ErrNotPlaceholder if path is a plain file, ErrIsDirectory for any
// directory at all (see the sentinel).
//
// Never call this on a file holding unsynced local content: the bit is what
// keeps that upload alive, and clearing the debt without paying it lets the
// next refresh or "free up space" dehydrate the only copy of the edit away.
func SetInSync(path string) error {
	attrs, _, err := findAttrTag(path)
	if err != nil {
		return err
	}
	// A DIRECTORY never gets the bit from here. The filter couples a
	// directory's in-sync state to its population, so one marked in-sync
	// enumerates EMPTY forever (TestInSyncDirStillPopulates) — the reason our
	// directory placeholders are created not in sync at all. Refusing it in
	// the call means no future caller has to remember.
	if attrs&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return ErrIsDirectory
	}
	h, err := openForCloud(path)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	var usn int64
	hr, _, _ := procCfSetInSyncState.Call(uintptr(h), uintptr(cfInSyncStateInSync), uintptr(cfSetInSyncFlagNone), uintptr(unsafe.Pointer(&usn)))
	if uint32(hr) == hrNotACloudFile {
		return ErrNotPlaceholder
	}
	if int32(hr) < 0 {
		return fmt.Errorf("CfSetInSyncState: 0x%08x", uint32(hr))
	}
	return nil
}

// PlaceholderIdentity returns the file identity stamped on a placeholder —
// for Nimbo's placeholders, the RAW server path hydration fetches from. It is
// what tells a placeholder that was MOVED to a new folder apart from a new
// file: its identity still names the old path. ErrNotPlaceholder for a plain
// item.
func PlaceholderIdentity(path string) ([]byte, error) {
	buf, err := standardInfo(path)
	if err != nil {
		return nil, err
	}
	info := (*placeholderStandardInfo)(unsafe.Pointer(&buf[0]))
	n := int(info.FileIdentityLength)
	start := int(unsafe.Offsetof(info.FileIdentity))
	if n < 0 || start+n > len(buf) {
		return nil, fmt.Errorf("CfGetPlaceholderInfo: identity length %d out of range", n)
	}
	return append([]byte(nil), buf[start:start+n]...), nil
}

// --- Pin / free-up-space (CfSetPinState + CfDehydratePlaceholder) ---

const (
	cfPinStateUnspecified = 0 // CF_PIN_STATE_UNSPECIFIED — no user preference
	cfPinStatePinned      = 1 // CF_PIN_STATE_PINNED — always keep on device
	cfPinStateUnpinned    = 2 // CF_PIN_STATE_UNPINNED — online-only preference
	cfSetPinFlagRecurse   = 1 // CF_SET_PIN_FLAG_RECURSE — apply to a directory tree
	cfDehydrateFlagNone   = 0
)

// openForCloud opens a handle suitable for cloud-STATE (metadata) operations —
// BACKUP_SEMANTICS so directories can be opened too, and deliberately
// ATTRIBUTES-ONLY access, not data access. Measured live 2026-09-14 (Windows
// 10.0.26200): opening a dehydrated placeholder for ANY data access —
// GENERIC_READ alone is enough, GENERIC_WRITE isn't required — makes the
// cloud filter hydrate it on the open itself, before any cfapi call runs and
// whether or not one follows. FILE_READ_ATTRIBUTES|FILE_WRITE_ATTRIBUTES never
// triggers this, and CfGetPlaceholderInfo, CfSetPinState, CfUpdatePlaceholder,
// CfConvertToPlaceholder (even converting a plain file with real local
// content), CfDehydratePlaceholder, CfHydratePlaceholder and
// CfRevertPlaceholder all accept a handle opened this way
// (FILE_FLAG_OPEN_REPARSE_POINT made no measured difference either way, so
// it's omitted). Before this, every metadata-only call here on an
// online-only file silently downloaded the whole file first as a side effect
// of the open.
//
// CfRevertPlaceholder was the last of those confirmed (2026-09-15, the
// revert leg of TestMetadataOpensDoNotHydrate): Microsoft's docs contradict
// themselves about it — the CfRevertPlaceholder page says an attribute
// handle suffices, a remark elsewhere mentions WRITE_DATA — and live it
// reverts a hydrated placeholder through this handle with its data intact.
func openForCloud(path string) (windows.Handle, error) {
	pathW, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(pathW,
		windows.FILE_READ_ATTRIBUTES|windows.FILE_WRITE_ATTRIBUTES,
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
	// Already dehydrated = done. This guard is NOT an optimisation: skipping it
	// used to make the filter FETCH the full content first and then discard it
	// (measured live, TestDehydrateAlreadyDehydratedDoesNotFetch) — but the
	// culprit was opening the handle via openForCloud with data access
	// (GENERIC_READ|GENERIC_WRITE), which itself hydrated the file before
	// CfDehydratePlaceholder ever ran; it was never CfDehydratePlaceholder
	// refetching a dataless placeholder. openForCloud is attrs-only now (see
	// its doc comment), so the open alone no longer re-downloads — this guard
	// still saves that open (and the round trip through cfapi) on files that
	// don't need it, so it stays.
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

// FILE_ATTRIBUTE_RECALL_ON_DATA_ACCESS: the placeholder has no local data —
// the online-only state Explorer draws with the cloud glyph.
const fileAttrRecallOnDataAccess = 0x00400000

const cfHydrateFlagNone = 0 // CF_HYDRATE_FLAG_NONE

// Hydrate downloads a placeholder's full content through the provider (the
// same FETCH_DATA path an application read takes), leaving a hydrated
// placeholder behind. A no-op on a file that already has its data.
func Hydrate(path string) error {
	h, err := openForCloud(path)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	// StartingOffset 0, Length -1 (to EOF), passed by value as LARGE_INTEGER —
	// the same convention Dehydrate uses.
	hr, _, _ := procCfHydratePlaceholder.Call(uintptr(h), 0, ^uintptr(0), uintptr(cfHydrateFlagNone), 0)
	if int32(hr) < 0 {
		return fmt.Errorf("CfHydratePlaceholder: 0x%08x", uint32(hr))
	}
	return nil
}

// PinnedDehydrated reports whether path is a FILE placeholder the user pinned
// ("Always keep on this device") that still has no local data — the state
// Explorer shows as "sync pending" until somebody downloads it. Directories
// carry the pin only as the recursive preference marker and never qualify.
func PinnedDehydrated(path string) bool {
	attrs, tag, err := findAttrTag(path)
	if err != nil || attrs&fileAttrDirectory != 0 {
		return false
	}
	if attrs&fileAttrPinned == 0 || attrs&fileAttrRecallOnDataAccess == 0 {
		return false
	}
	r1, _, _ := procCfGetPlaceholderStateFromAttrTag.Call(uintptr(attrs), uintptr(tag))
	state := uint32(r1)
	return state != cfPlaceholderStateInvalid && state&cfPlaceholderStatePlaceholder != 0
}

// DirPopulated reports whether a DIRECTORY already holds everything the server
// has for it — i.e. whether an empty listing on disk means "empty" or "not
// fetched yet".
//
// A directory placeholder is created lazily: the shell issues
// FETCH_PLACEHOLDERS the first time it is opened, and the transfer that answers
// passes DISABLE_ON_DEMAND_POPULATION whenever the listing SUCCEEDED — zero
// entries included. That clears FILE_ATTRIBUTE_RECALL_ON_DATA_ACCESS from the
// directory and the flag is permanent: the shell never asks again. So a
// directory that was populated EMPTY looks exactly like one that was never
// populated if you only count its entries, and anything the server adds inside
// it afterwards has nobody left to fetch it — which is why reconcile has to
// ask this question instead (measured on the VM, build 0.1.0.284: a populated
// /Notes at attrs 0x100410 never showed a subfolder added on another device).
//
// A plain directory has no such state and is always populated: everything in
// it is real. A non-directory answers false — there is nothing to populate.
// FindFirstFile only, like the other attribute probes: no handle, no open, and
// nothing that could hydrate anything.
func DirPopulated(path string) (bool, error) {
	attrs, _, err := findAttrTag(path)
	if err != nil {
		return false, err
	}
	if attrs&fileAttrDirectory == 0 {
		return false, nil
	}
	return attrs&fileAttrRecallOnDataAccess == 0, nil
}

// CF_UPDATE_FLAG_DISABLE_ON_DEMAND_POPULATION is 0x10 (cfapi.h 10.0.26100).
// Not 0x20: that is REMOVE_FILE_IDENTITY, which is refused on a directory
// with 0x8007017C and once led us to believe this call only works inside the
// population callback. It works from anywhere (measured 2026-09-25).
const cfUpdateFlagDisableOnDemandPopulation = 0x00000010

// MarkDirPopulated records that the provider has itself created every child of
// a directory placeholder, the way a FETCH_PLACEHOLDERS transfer would have:
// the directory stops being "not fetched yet" (RECALL_ON_DATA_ACCESS and
// PARTIAL clear, so the shell never asks for it) and is marked in sync, which
// is what takes the "sync pending" arrows off it. Nothing is enumerating it on
// our behalf, so there is no pending enumeration for the in-sync bit to
// poison. Measured on the live driver: it works outside the callback, the
// bits survive a child being created or hydrated afterwards, and children
// created in a pinned directory inherit the pin.
//
// Call it only once every child is in place. The flag is permanent: a
// directory marked with entries missing is never asked for them again, and
// only reconcile's listing would bring them in.
func MarkDirPopulated(path string) error {
	h, err := openForCloud(path)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	var usn int64
	hr, _, _ := procCfUpdatePlaceholder.Call(
		uintptr(h),
		0,    // FsMetadata (unchanged)
		0, 0, // identity (unchanged)
		0, 0, // no dehydrate ranges
		uintptr(cfUpdateFlagDisableOnDemandPopulation|cfUpdateFlagMarkInSync),
		uintptr(unsafe.Pointer(&usn)),
		0, // Overlapped
	)
	if int32(hr) < 0 {
		return fmt.Errorf("CfUpdatePlaceholder(populated): 0x%08x", uint32(hr))
	}
	return nil
}

// Pinned reports whether path carries the user's "always keep on this device"
// preference. Attributes only, like PinStateOf.
func Pinned(path string) bool { return PinStateOf(path) == "pinned" }

// HydrateIfPinned completes the pin contract for one file: Explorer's own
// verb hydrates as it pins, but Nimbo's context-menu entry and pins applied
// while Nimbo wasn't running only set the attribute (issue #7). Returns true
// when it downloaded the file.
func HydrateIfPinned(path string) (bool, error) {
	if !PinnedDehydrated(path) {
		return false, nil
	}
	if err := Hydrate(path); err != nil {
		return false, err
	}
	ShellNotifyUpdated(path)
	return true, nil
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
