//go:build windows

package cfapi

// Shell sync-root registration via the brokered WinRT API.
//
// Explorer reads cloud-provider metadata from
//
//	HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\Explorer\SyncRootManager
//
// A packaged (MSIX) app cannot write HKLM at all — its registry writes are
// virtualised into the package's private hive — so the hand-written registry
// path in cfapi_windows.go could only ever create an HKCU key that Windows does
// not read. That is why Nimbo has never shown status icons on an installed
// build, in either sync mode.
//
// StorageProviderSyncRootManager.Register is the supported way in: it is a
// brokered API, so the *system* writes HKLM on the caller's behalf after
// checking the caller's package identity. It is what OneDrive uses, which is why
// OneDrive's entry appears under HKLM despite OneDrive also being packaged.
//
// There is no Go binding for it, so this is hand-rolled vtable interop. Every
// IID and every method index below was read out of the Windows SDK IDL
// (Include\<ver>\winrt\windows.storage.provider.idl and windows.storage.idl)
// rather than recalled — a wrong index here calls the wrong function with the
// wrong arguments, which is not a failure mode worth guessing at.

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/go-ole/go-ole"
	"golang.org/x/sys/windows"
)

// WinRT runtime class names (activation ids).
const (
	classStorageFolder   = "Windows.Storage.StorageFolder"
	classCryptoBuffer    = "Windows.Security.Cryptography.CryptographicBuffer"
	classSyncRootInfo    = "Windows.Storage.Provider.StorageProviderSyncRootInfo"
	classSyncRootManager = "Windows.Storage.Provider.StorageProviderSyncRootManager"
)

// Interface ids, verbatim from the SDK IDL.
const (
	iidStorageFolderStatics   = "{08F327FF-85D5-48B9-AEE9-28511E339F9F}"
	iidStorageFolder          = "{72D1CB78-B3EF-4F75-A80B-6FD9DAE2944B}"
	iidCryptoBufferStatics    = "{320B7E22-3CB0-4CDF-8663-1D28910065EB}"
	iidSyncRootInfo           = "{7C1305C4-99F9-41AC-8904-AB055D654926}"
	iidSyncRootManagerStatics = "{3E99FBBF-8FE3-4B40-ABC7-F6FC3D74C98E}"
	iidAsyncInfo              = "{00000036-0000-0000-C000-000000000046}"
)

// Vtable slots. Every WinRT interface begins with IInspectable's six entries
// (QueryInterface/AddRef/Release/GetIids/GetRuntimeClassName/GetTrustLevel), so
// the interface's own methods start at 6, in IDL declaration order. Property
// pairs are getter-then-setter.
const (
	idxQueryInterface = 0
	idxRelease        = 2

	// IStorageFolderStatics
	idxGetFolderFromPathAsync = 6

	// IAsyncOperation<T>: put_Completed, get_Completed, GetResults
	idxAsyncOpGetResults = 8

	// IAsyncInfo: get_Id, get_Status, get_ErrorCode, Cancel, Close
	idxAsyncInfoStatus    = 7
	idxAsyncInfoErrorCode = 8
	idxAsyncInfoClose     = 10

	// ICryptographicBufferStatics
	idxCreateFromByteArray = 9

	// IStorageProviderSyncRootInfo (setters only; getters are the even slots)
	idxPutID                      = 7
	idxPutContext                 = 9
	idxPutPath                    = 11
	idxPutDisplayNameResource     = 13
	idxPutIconResource            = 15
	idxPutHydrationPolicy         = 17
	idxPutHydrationPolicyModifier = 19
	idxPutPopulationPolicy        = 21
	idxPutInSyncPolicy            = 23
	idxPutHardlinkPolicy          = 25
	idxPutShowSiblingsAsGroup     = 27
	idxPutVersion                 = 29
	idxPutProtectionMode          = 31
	idxPutAllowPinning            = 33

	// IStorageProviderSyncRootManagerStatics
	idxRegister   = 6
	idxUnregister = 7
)

// AsyncStatus.
const (
	asyncStarted   = 0
	asyncCompleted = 1
)

// ShellPolicy is what Explorer is told about a root.
//
// These are the WinRT StorageProvider* enum values, which are NOT the CF_*
// numbering used for CfRegisterSyncRoot — StorageProviderPopulationPolicy has no
// "partial" member at all, and its Full is 1 where CF_POPULATION_POLICY's Full
// is 2. Mixing the two silently describes the root wrongly, which is what the
// old hand-written registry values did.
type ShellPolicy struct {
	Hydration    uint32
	Population   uint32
	InSync       uint32
	AllowPinning bool
	ShowSiblings bool
}

var (
	// ShellPolicyOnDemand pairs with RegisterSyncRoot: files may be online-only
	// and directories populate on demand. Pinning is offered because "Always keep
	// on this device" is meaningful only when a file can be online-only.
	ShellPolicyOnDemand = ShellPolicy{Hydration: 2 /*Full*/, Population: 1 /*Full*/, AllowPinning: true}

	// ShellPolicyStatusOnly pairs with RegisterStatusRoot: everything is always
	// present locally, so nothing may ever be dehydrated or left unpopulated —
	// and pinning is not offered, since there is nothing to free up.
	ShellPolicyStatusOnly = ShellPolicy{Hydration: 3 /*AlwaysFull*/, Population: 2 /*AlwaysFull*/}
)

// --- minimal COM plumbing ---

// comObj is a raw WinRT interface pointer. The pointer is held as an
// unsafe.Pointer rather than a uintptr so no uintptr→pointer conversion is ever
// needed: out-parameters are declared as unsafe.Pointer and the callee writes
// straight into them.
type comObj struct{ p unsafe.Pointer }

func (c comObj) ok() bool { return c.p != nil }

func (c comObj) call(idx int, args ...uintptr) uintptr {
	vtbl := *(**[64]uintptr)(c.p)
	all := make([]uintptr, 0, len(args)+1)
	all = append(all, uintptr(c.p))
	all = append(all, args...)
	hr, _, _ := syscall.SyscallN(vtbl[idx], all...)
	return hr
}

func (c comObj) release() {
	if c.p != nil {
		c.call(idxRelease)
	}
}

func (c comObj) queryInterface(iid string) (comObj, error) {
	var out unsafe.Pointer
	guid := ole.NewGUID(iid)
	hr := c.call(idxQueryInterface,
		uintptr(unsafe.Pointer(guid)),
		uintptr(unsafe.Pointer(&out)),
	)
	runtime.KeepAlive(guid)
	if hr != 0 {
		return comObj{}, fmt.Errorf("QueryInterface(%s): %w", iid, hresult(hr))
	}
	return comObj{p: out}, nil
}

func hresult(hr uintptr) error { return ole.NewError(hr) }

var (
	combase                 = windows.NewLazySystemDLL("combase.dll")
	procWindowsCreateString = combase.NewProc("WindowsCreateString")
	procWindowsDeleteString = combase.NewProc("WindowsDeleteString")
)

// hstr is an owned HSTRING. Sized in UTF-16 code units, not runes, so a path or
// display name containing non-BMP characters survives intact (go-ole's
// NewHString gets this wrong, which is why it is not used here).
type hstr struct{ h ole.HString }

func newHStr(s string) (hstr, error) {
	u16 := append(utf16.Encode([]rune(s)), 0)
	var h ole.HString
	hr, _, _ := procWindowsCreateString.Call(
		uintptr(unsafe.Pointer(&u16[0])),
		uintptr(len(u16)-1),
		uintptr(unsafe.Pointer(&h)),
	)
	runtime.KeepAlive(u16)
	if hr != 0 {
		return hstr{}, hresult(hr)
	}
	return hstr{h: h}, nil
}

func (s hstr) free() {
	if s.h != 0 {
		_, _, _ = procWindowsDeleteString.Call(uintptr(s.h))
	}
}

func factory(class, iid string) (comObj, error) {
	insp, err := ole.RoGetActivationFactory(class, ole.NewGUID(iid))
	if err != nil {
		return comObj{}, fmt.Errorf("activation factory %s: %w", class, err)
	}
	return comObj{p: unsafe.Pointer(insp)}, nil
}

// awaitOperation blocks until an IAsyncOperation finishes and returns its result.
//
// Polling IAsyncInfo::Status is deliberate: the alternative is implementing an
// AsyncOperationCompletedHandler COM object in Go, which means a synthesised
// vtable and callbacks arriving on an arbitrary thread. For one folder lookup
// that finishes in microseconds, a poll is the smaller risk.
func awaitOperation(op comObj, timeout time.Duration) (comObj, error) {
	info, err := op.queryInterface(iidAsyncInfo)
	if err != nil {
		return comObj{}, err
	}
	defer info.release()

	deadline := time.Now().Add(timeout)
	for {
		var status uint32
		if hr := info.call(idxAsyncInfoStatus, uintptr(unsafe.Pointer(&status))); hr != 0 {
			return comObj{}, fmt.Errorf("IAsyncInfo::Status: %w", hresult(hr))
		}
		if status == asyncCompleted {
			break
		}
		if status != asyncStarted {
			var code uint32
			info.call(idxAsyncInfoErrorCode, uintptr(unsafe.Pointer(&code)))
			info.call(idxAsyncInfoClose)
			return comObj{}, fmt.Errorf("async operation failed (status %d): 0x%08x", status, code)
		}
		if time.Now().After(deadline) {
			info.call(idxAsyncInfoClose)
			return comObj{}, errors.New("async operation timed out")
		}
		time.Sleep(2 * time.Millisecond)
	}

	var res unsafe.Pointer
	if hr := op.call(idxAsyncOpGetResults, uintptr(unsafe.Pointer(&res))); hr != 0 {
		return comObj{}, fmt.Errorf("GetResults: %w", hresult(hr))
	}
	return comObj{p: res}, nil
}

// --- the dedicated apartment thread ---
//
// COM apartment state is per-thread, and Go moves goroutines between threads
// freely, so every WinRT call is funnelled onto one thread that is initialised
// once and never released. This mirrors the toast raiser in internal/notify,
// which exists for the same reason.

type winrtJob struct {
	fn    func() error
	reply chan error
}

var (
	winrtOnce sync.Once
	winrtCh   chan winrtJob
	winrtErr  error
)

const roInitMultithreaded = 1

func winrtDo(fn func() error) error {
	winrtOnce.Do(func() {
		winrtCh = make(chan winrtJob)
		ready := make(chan struct{})
		go winrtLoop(ready)
		<-ready
	})
	if winrtErr != nil {
		return winrtErr
	}
	job := winrtJob{fn: fn, reply: make(chan error, 1)}
	winrtCh <- job
	return <-job.reply
}

func winrtLoop(ready chan struct{}) {
	runtime.LockOSThread() // never unlocked; this thread lives for the process
	if err := ole.RoInitialize(roInitMultithreaded); err != nil {
		// RPC_E_CHANGED_MODE means the thread already belongs to an apartment,
		// which is still perfectly usable. Anything else is fatal for WinRT.
		var oleErr *ole.OleError
		if !errors.As(err, &oleErr) || uintptr(oleErr.Code()) != 0x80010106 {
			winrtErr = fmt.Errorf("RoInitialize: %w", err)
			close(ready)
			for job := range winrtCh { // keep draining so callers never block
				job.reply <- winrtErr
			}
			return
		}
	}
	close(ready)
	for job := range winrtCh {
		job.reply <- job.fn()
	}
}

// --- the registration itself ---

// registerShellSyncRootWinRT registers path with Explorer through the brokered
// API, so the metadata lands in HKLM where Windows actually reads it.
func registerShellSyncRootWinRT(id, path, displayName, iconResource, version string, pol ShellPolicy) error {
	return winrtDo(func() error {
		folder, err := storageFolderFromPath(path)
		if err != nil {
			return err
		}
		defer folder.release()

		// Context is a required property: Register rejects the info object
		// without one. Its content is opaque to Windows and handed back to the
		// provider, so the root path serves as a self-describing value.
		ctx, err := bufferFromBytes([]byte(path))
		if err != nil {
			return err
		}
		defer ctx.release()

		insp, err := ole.RoActivateInstance(classSyncRootInfo)
		if err != nil {
			return fmt.Errorf("activate %s: %w", classSyncRootInfo, err)
		}
		base := comObj{p: unsafe.Pointer(insp)}
		info, err := base.queryInterface(iidSyncRootInfo)
		base.release()
		if err != nil {
			return err
		}
		defer info.release()

		if err := setStr(info, idxPutID, "Id", id); err != nil {
			return err
		}
		if hr := info.call(idxPutPath, uintptr(folder.p)); hr != 0 {
			return fmt.Errorf("put_Path: %w", hresult(hr))
		}
		if err := setStr(info, idxPutDisplayNameResource, "DisplayNameResource", displayName); err != nil {
			return err
		}
		if err := setStr(info, idxPutIconResource, "IconResource", iconResource); err != nil {
			return err
		}
		if err := setStr(info, idxPutVersion, "Version", version); err != nil {
			return err
		}
		if hr := info.call(idxPutContext, uintptr(ctx.p)); hr != 0 {
			return fmt.Errorf("put_Context: %w", hresult(hr))
		}
		for _, p := range []struct {
			idx  int
			name string
			val  uintptr
		}{
			{idxPutHydrationPolicy, "HydrationPolicy", uintptr(pol.Hydration)},
			{idxPutHydrationPolicyModifier, "HydrationPolicyModifier", 0},
			{idxPutPopulationPolicy, "PopulationPolicy", uintptr(pol.Population)},
			{idxPutInSyncPolicy, "InSyncPolicy", uintptr(pol.InSync)},
			{idxPutHardlinkPolicy, "HardlinkPolicy", 0},
			{idxPutShowSiblingsAsGroup, "ShowSiblingsAsGroup", boolArg(pol.ShowSiblings)},
			{idxPutAllowPinning, "AllowPinning", boolArg(pol.AllowPinning)},
			{idxPutProtectionMode, "ProtectionMode", 1 /*Personal*/},
		} {
			if hr := info.call(p.idx, p.val); hr != 0 {
				return fmt.Errorf("put_%s: %w", p.name, hresult(hr))
			}
		}

		// DO NOT append StorageProviderItemPropertyDefinitions here. v0.1.0.245
		// did (raw-ABI vector append, worked mechanically — they landed as
		// SyncRootManager\<id>\CustomStates\<n>), and their mere presence made
		// Windows 11 24H2's Explorer (26100.9168, Windows.FileExplorer.Common
		// 26100.8972) crash with 0xc0000005 at a fixed offset — on registration,
		// while the desktop was LOCKED, and again on every folder-window open.
		// Not even OneDrive ships CustomStates; the mechanism is bit-rotted.
		// The CustomStateHandler's properties carry their own IconResource, so
		// definitions are not needed to render. If they ever seem needed again,
		// experiment via direct registry writes on the test VM — never a release.

		mgr, err := factory(classSyncRootManager, iidSyncRootManagerStatics)
		if err != nil {
			return err
		}
		defer mgr.release()
		if hr := mgr.call(idxRegister, uintptr(info.p)); hr != 0 {
			return fmt.Errorf("StorageProviderSyncRootManager.Register: %w", hresult(hr))
		}
		return nil
	})
}

// unregisterShellSyncRootWinRT removes a registration made by the brokered API.
func unregisterShellSyncRootWinRT(id string) error {
	return winrtDo(func() error {
		mgr, err := factory(classSyncRootManager, iidSyncRootManagerStatics)
		if err != nil {
			return err
		}
		defer mgr.release()
		s, err := newHStr(id)
		if err != nil {
			return err
		}
		defer s.free()
		if hr := mgr.call(idxUnregister, uintptr(s.h)); hr != 0 {
			return fmt.Errorf("StorageProviderSyncRootManager.Unregister: %w", hresult(hr))
		}
		return nil
	})
}

// boolArg encodes a WinRT `boolean`, which is one byte on the ABI.
func boolArg(b bool) uintptr {
	if b {
		return 1
	}
	return 0
}

func setStr(o comObj, idx int, name, value string) error {
	s, err := newHStr(value)
	if err != nil {
		return fmt.Errorf("put_%s: %w", name, err)
	}
	defer s.free()
	if hr := o.call(idx, uintptr(s.h)); hr != 0 {
		return fmt.Errorf("put_%s: %w", name, hresult(hr))
	}
	return nil
}

// storageFolderFromPath resolves a filesystem path to the IStorageFolder that
// StorageProviderSyncRootInfo.Path requires.
func storageFolderFromPath(path string) (comObj, error) {
	f, err := factory(classStorageFolder, iidStorageFolderStatics)
	if err != nil {
		return comObj{}, err
	}
	defer f.release()

	s, err := newHStr(path)
	if err != nil {
		return comObj{}, err
	}
	defer s.free()

	var op unsafe.Pointer
	if hr := f.call(idxGetFolderFromPathAsync, uintptr(s.h), uintptr(unsafe.Pointer(&op))); hr != 0 {
		return comObj{}, fmt.Errorf("GetFolderFromPathAsync(%s): %w", path, hresult(hr))
	}
	operation := comObj{p: op}
	defer operation.release()

	res, err := awaitOperation(operation, 10*time.Second)
	if err != nil {
		return comObj{}, fmt.Errorf("GetFolderFromPathAsync(%s): %w", path, err)
	}
	// StorageFolder's default interface is IStorageFolder, so the result is
	// already the right pointer; the QI keeps that an assertion rather than an
	// assumption.
	folder, err := res.queryInterface(iidStorageFolder)
	res.release()
	if err != nil {
		return comObj{}, err
	}
	return folder, nil
}

// bufferFromBytes wraps bytes in an IBuffer via CryptographicBuffer, which is
// far less work than implementing IBuffer in Go.
func bufferFromBytes(b []byte) (comObj, error) {
	if len(b) == 0 {
		b = []byte{0}
	}
	f, err := factory(classCryptoBuffer, iidCryptoBufferStatics)
	if err != nil {
		return comObj{}, err
	}
	defer f.release()

	var out unsafe.Pointer
	hr := f.call(idxCreateFromByteArray,
		uintptr(len(b)),
		uintptr(unsafe.Pointer(&b[0])),
		uintptr(unsafe.Pointer(&out)),
	)
	runtime.KeepAlive(b)
	if hr != 0 {
		return comObj{}, fmt.Errorf("CreateFromByteArray: %w", hresult(hr))
	}
	return comObj{p: out}, nil
}
