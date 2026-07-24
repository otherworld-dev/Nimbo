//go:build windows

// Package cfapi is an experimental integration with the Windows Cloud Files API
// (cldapi.dll) that powers on-demand ("online-only") files. This first layer
// registers and unregisters a folder as a cloud sync root — the foundation the
// placeholder + hydration layers build on. It is opt-in and non-destructive:
// registering a sync root does not alter the files inside it.
package cfapi

import (
	"fmt"
	"hash/fnv"
	"os"
	"os/user"
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
	cldapi                   = windows.NewLazySystemDLL("cldapi.dll")
	procCfRegisterSyncRoot   = cldapi.NewProc("CfRegisterSyncRoot")
	procCfUnregisterSyncRoot = cldapi.NewProc("CfUnregisterSyncRoot")
	procCfConnectSyncRoot    = cldapi.NewProc("CfConnectSyncRoot")
	procCfDisconnectSyncRoot = cldapi.NewProc("CfDisconnectSyncRoot")
	procCfCreatePlaceholders = cldapi.NewProc("CfCreatePlaceholders")
	procCfExecute            = cldapi.NewProc("CfExecute")
	procCfConvertToPlaceholder            = cldapi.NewProc("CfConvertToPlaceholder")
	procCfSetInSyncState                  = cldapi.NewProc("CfSetInSyncState")
	procCfGetPlaceholderStateFromAttrTag  = cldapi.NewProc("CfGetPlaceholderStateFromAttributeTag")
	procCfUpdatePlaceholder               = cldapi.NewProc("CfUpdatePlaceholder")
	procCfSetPinState                     = cldapi.NewProc("CfSetPinState")
	procCfDehydratePlaceholder            = cldapi.NewProc("CfDehydratePlaceholder")
)

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
	_                      uint32
	ProviderID             windows.GUID
}

const (
	cfHydrationPolicyFull    = 2 // CF_HYDRATION_POLICY_PRIMARY_FULL
	cfPopulationPolicyPartial = 0 // CF_POPULATION_POLICY_PRIMARY_PARTIAL (on-demand dirs)
	cfRegisterFlagUpdate     = 0x00000001
	cfRegisterFlagDisableOnDemandPopulationOnRoot = 0x00000002
)

// RegisterSyncRoot registers path as a Nimbo cloud sync root. Idempotent (uses
// the UPDATE flag). The identity is an opaque per-root blob (we use the path).
func RegisterSyncRoot(path string) error {
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
		Hydration:  hydrationPolicy{Primary: cfHydrationPolicyFull},
		Population: populationPolicy{Primary: cfPopulationPolicyPartial},
	}
	pol.StructSize = uint32(unsafe.Sizeof(pol))

	// DisableOnDemandPopulationOnRoot: we seed the root's top level eagerly, so
	// the filter must NOT ask us to populate the root (that request would time
	// out — exactly the failure seen before). Subdirectories still populate
	// on-demand via FETCH_PLACEHOLDERS.
	hr, _, _ := procCfRegisterSyncRoot.Call(
		uintptr(unsafe.Pointer(pathW)),
		uintptr(unsafe.Pointer(&reg)),
		uintptr(unsafe.Pointer(&pol)),
		uintptr(cfRegisterFlagUpdate|cfRegisterFlagDisableOnDemandPopulationOnRoot),
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
// write the HKCU SyncRootManager metadata that Explorer needs to render a cloud
// folder (display name, icon, policies). Without it, Explorer crashes rendering
// the folder. The WinRT StorageProviderSyncRootManager.Register API writes these
// keys; we replicate them directly in the registry.

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

// RegisterShellSyncRoot writes the SyncRootManager metadata so Explorer can
// present the sync root at path. displayName/iconPath are shown in the UI.
func RegisterShellSyncRoot(path, displayName, iconPath string) error {
	id, err := shellRootID(path)
	if err != nil {
		return err
	}
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
	_ = k.SetDWordValue("HydrationPolicy", 0)         // Partial
	_ = k.SetDWordValue("HydrationPolicyModifier", 0) //
	_ = k.SetDWordValue("PopulationPolicy", 2)        // Full (we create placeholders)
	_ = k.SetDWordValue("InSyncPolicy", 0)
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

// UnregisterShellSyncRoot removes the SyncRootManager metadata for path.
func UnregisterShellSyncRoot(path string) {
	id, err := shellRootID(path)
	if err != nil {
		return
	}
	base := syncRootManager + `\` + id
	_ = registry.DeleteKey(registry.CURRENT_USER, base+`\UserSyncRoots`)
	_ = registry.DeleteKey(registry.CURRENT_USER, base)
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
	providers             sync.Map // connKey int64 -> *provider
	fetchDataCallbackPtr   = syscall.NewCallback(fetchDataCallback)
	fetchPlaceholdersPtr   = syscall.NewCallback(fetchPlaceholdersCallback)
)

// CF_CALLBACK_REGISTRATION { CF_CALLBACK_TYPE Type; CF_CALLBACK Callback; }
type callbackRegistration struct {
	Type     int32
	_        int32
	Callback uintptr
}

const (
	cfCallbackTypeFetchData         = 0
	cfCallbackTypeFetchPlaceholders = 3
	cfCallbackTypeNone              = -1
	cfConnectFlagNone               = 0
	cfOperationTypeTransferData         = 0
	cfOperationTypeTransferPlaceholders = 4
)

// Mount registers path as a sync root and connects a provider that hydrates
// files (hydrate) and populates directories on demand (list). Returns a
// connection key used to unmount.
func Mount(path, displayName, iconPath string, hydrate HydrateFunc, list ListFunc) (int64, error) {
	if err := RegisterSyncRoot(path); err != nil {
		return 0, err
	}
	if err := RegisterShellSyncRoot(path, displayName, iconPath); err != nil {
		_ = UnregisterSyncRoot(path)
		return 0, err
	}
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
		uintptr(cfConnectFlagNone),
		uintptr(unsafe.Pointer(&connKey)),
	)
	runtime.KeepAlive(table)
	if int32(hr) < 0 {
		UnregisterShellSyncRoot(path)
		_ = UnregisterSyncRoot(path)
		return 0, fmt.Errorf("CfConnectSyncRoot: 0x%08x", uint32(hr))
	}
	providers.Store(connKey, &provider{path: path, hydrate: hydrate, list: list})

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
func Unmount(path string, connKey int64) {
	providers.Delete(connKey)
	_, _, _ = procCfDisconnectSyncRoot.Call(uintptr(connKey))
	UnregisterShellSyncRoot(path)
	_ = UnregisterSyncRoot(path)
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
	fileAttrNormal                              = 0x00000080
	fileAttrDirectory                           = 0x00000010
	cfPlaceholderCreateFlagDisableOnDemandPopulation = 0x00000001
	cfPlaceholderCreateFlagMarkInSync                = 0x00000002
	cfCreateFlagNone                            = 0
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
	// CF_CALLBACK_PARAMETERS (FetchData) offsets.
	cpRequiredOffset = 16
	cpRequiredLength = 24
)

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
	dbg("FETCH_DATA conn=%d transfer=%d idLen=%d offset=%d length=%d", connKey, transferKey, idLen, reqOffset, reqLength)

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
	StructSize       uint32
	Type             uint32
	ConnectionKey    int64
	TransferKey      int64
	RequestKey       int64
	CorrelationVector uintptr
	SyncStatus       uintptr
}

// CF_OPERATION_PARAMETERS (TransferData)
type opParamsTransfer struct {
	ParamSize        uint32
	Flags            uint32
	CompletionStatus int32
	_                uint32
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

	go func() {
		items := p.list(rel)
		dbg("FETCH_PLACEHOLDERS rel=%q -> %d entries", rel, len(items))
		cfTransferPlaceholders(connKey, transferKey, items)
	}()
	return 0
}

// CF_OPERATION_PARAMETERS (TransferPlaceholders)
type opParamsTransferPlaceholders struct {
	ParamSize             uint32
	Flags                 uint32
	CompletionStatus      int32
	_                     uint32
	PlaceholderTotalCount int64
	PlaceholderArray      uintptr
	PlaceholderCount      uint32
	EntriesProcessed      uint32
}

// 0x1 is STOP_ON_ERROR; DISABLE_ON_DEMAND_POPULATION (mark the dir fully
// populated so the shell stops re-requesting it) is 0x2.
const cfOpTransferPlaceholdersFlagDisableOnDemandPopulation = 0x00000002

func cfTransferPlaceholders(connKey, transferKey int64, items []PlaceholderInfo) {
	arr, names, ids, err := buildPlaceholders(items)
	if err != nil {
		dbg("TRANSFER_PLACEHOLDERS build error: %v", err)
		return
	}
	oi := operationInfo{Type: cfOperationTypeTransferPlaceholders, ConnectionKey: connKey, TransferKey: transferKey}
	oi.StructSize = uint32(unsafe.Sizeof(oi))
	op := opParamsTransferPlaceholders{
		Flags:                 cfOpTransferPlaceholdersFlagDisableOnDemandPopulation,
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
	if int32(hr) < 0 {
		return fmt.Errorf("CfConvertToPlaceholder: 0x%08x", uint32(hr))
	}
	return nil
}

// CF_UPDATE_FLAG_MARK_IN_SYNC is 0x2 (0x1 is VERIFY_IN_SYNC, which fails when
// the file isn't already in sync — not what we want after a rename).
const cfUpdateFlagMarkInSync = 0x00000002

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
	cfPinStatePinned    = 1 // CF_PIN_STATE_PINNED — always keep on device
	cfPinStateUnpinned  = 2 // CF_PIN_STATE_UNPINNED — online-only preference
	cfSetPinFlagRecurse = 1 // CF_SET_PIN_FLAG_RECURSE — apply to a directory tree
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
		uintptr(cfUpdateFlagMarkInSync),
		uintptr(unsafe.Pointer(&usn)),
		0,
	)
	runtime.KeepAlive(identity)
	runtime.KeepAlive(meta)
	windows.CloseHandle(h)
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
