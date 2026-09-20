//go:build windows

package autostart

// The packaged path: Windows.ApplicationModel.StartupTask, called through the
// raw WinRT ABI with no cgo.
//
// Every IID, vtable slot and enum value below was read out of the Windows SDK
// 10.0.26100.0 headers on the dev machine, not recalled (this project has
// shipped three struct-layout bugs from recalled values):
//
//	Include\10.0.26100.0\winrt\windows.applicationmodel.h (+ .idl)
//	  IStartupTaskStatics {EE5B60BD-A148-41A7-B26E-E8B88A1E62F8}
//	    6 GetForCurrentPackageAsync, 7 GetAsync(HSTRING, IAsyncOperation<StartupTask>**)
//	  IStartupTask {F75C23C8-B5F2-4F6C-88DD-36CB1D599D17}  (StartupTask's default interface)
//	    6 RequestEnableAsync(IAsyncOperation<StartupTaskState>**), 7 Disable(),
//	    8 get_State(StartupTaskState*), 9 get_TaskId(HSTRING*)
//	  IAsyncOperation<StartupTask>      {CBEC7A4E-A046-5330-873D-0FCE228792FA}
//	  IAsyncOperation<StartupTaskState> {5239A934-80E2-518F-B819-1F316F379A3F}
//	    both: 6 put_Completed, 7 get_Completed, 8 GetResults — which writes an
//	    IStartupTask* for the first and an int32 enum for the second
//	Include\10.0.26100.0\winrt\asyncinfo.h
//	  IAsyncInfo {00000036-0000-0000-C000-000000000046}
//	    6 get_Id, 7 get_Status, 8 get_ErrorCode, 9 Cancel, 10 Close
//	  AsyncStatus: Started 0, Completed 1, Canceled 2, Error 3
//
// Slots 0-5 are IUnknown (QueryInterface, AddRef, Release) then IInspectable
// (GetIids, GetRuntimeClassName, GetTrustLevel), so an interface's own methods
// start at 6. The same IIDs and slot orders appear in the SDK's C++/WinRT
// projection (cppwinrt\winrt\impl\windows.applicationmodel.0.h), and the two
// parameterized IIDs also come out of the WinRT pinterface algorithm (UUIDv5
// over "pinterface({9fc2b0bb-...};<type signature>)"), so all three agree.
//
// The COM helpers mirror internal/cfapi/syncroot_winrt_windows.go (comObj,
// factory, newHStr, polling IAsyncInfo, one locked MTA thread). They are copied
// rather than shared on purpose: cfapi registers the sync root, and nothing
// changed for autostart should be able to break that. They also differ where
// it matters here: GetResults for an enum takes a *int32 rather than an
// interface pointer; every call has a deadline, so a stuck WinRT call cannot
// hang the Settings window; results travel back over a channel rather than a
// captured variable, so a call that outlives its deadline races with nothing;
// and call() is marked go:uintptrescapes (see there).

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

// startupTaskID must match the TaskId of the desktop:StartupTask in
// packaging/msix/AppxManifest.xml. White-label builds
// (packaging/whitelabel/build-partner.ps1) rewrite the task's DisplayName but
// keep this id.
const startupTaskID = "NimboAutostart"

// classStartupTask is the runtime class whose activation factory implements
// IStartupTaskStatics.
const classStartupTask = "Windows.ApplicationModel.StartupTask"

// Interface ids, verbatim from the SDK headers (see the top of the file).
const (
	iidStartupTaskStatics      = "{EE5B60BD-A148-41A7-B26E-E8B88A1E62F8}"
	iidStartupTask             = "{F75C23C8-B5F2-4F6C-88DD-36CB1D599D17}"
	iidAsyncOpStartupTask      = "{CBEC7A4E-A046-5330-873D-0FCE228792FA}"
	iidAsyncOpStartupTaskState = "{5239A934-80E2-518F-B819-1F316F379A3F}"
	iidAsyncInfo               = "{00000036-0000-0000-C000-000000000046}"
)

// Vtable slots, from the SDK headers' C Vtbl structs.
const (
	idxQueryInterface = 0
	idxRelease        = 2

	// IStartupTaskStatics
	idxGetAsync = 7

	// IStartupTask
	idxRequestEnableAsync = 6
	idxDisable            = 7
	idxGetState           = 8

	// IAsyncOperation<T>
	idxAsyncOpGetResults = 8

	// IAsyncInfo
	idxAsyncInfoStatus    = 7
	idxAsyncInfoErrorCode = 8
	idxAsyncInfoCancel    = 9
	idxAsyncInfoClose     = 10
)

// AsyncStatus, from asyncinfo.h.
const (
	asyncStarted   = 0
	asyncCompleted = 1
)

// callTimeout bounds every public call end to end, waiting for the WinRT
// thread included. The real calls take milliseconds. A var so tests can
// shorten it.
var callTimeout = 5 * time.Second

// --- the three operations autostart needs ---

// startupTaskState reads the task's current state.
func startupTaskState() (startupState, error) {
	return winrtCall(func() (startupState, error) {
		task, err := getStartupTask()
		if err != nil {
			return 0, err
		}
		defer task.release()
		return readState(task)
	})
}

// startupTaskRequestEnable asks Windows to turn the task on and returns the
// state it settled on. For a full-trust app there is no dialog: Windows
// enables it unless the user or a policy has turned it off, and says which.
func startupTaskRequestEnable() (startupState, error) {
	return winrtCall(func() (startupState, error) {
		task, err := getStartupTask()
		if err != nil {
			return 0, err
		}
		defer task.release()

		var raw unsafe.Pointer
		if hr := task.call(idxRequestEnableAsync, uintptr(unsafe.Pointer(&raw))); failed(hr) {
			return 0, fmt.Errorf("StartupTask.RequestEnableAsync: %w", hresult(hr))
		}
		if raw == nil {
			return 0, errors.New("StartupTask.RequestEnableAsync returned no operation")
		}
		opAny := comObj{p: raw}
		defer opAny.release()
		op, err := opAny.queryInterface(iidAsyncOpStartupTaskState)
		if err != nil {
			return 0, err
		}
		defer op.release()
		if err := waitAsync(op); err != nil {
			return 0, fmt.Errorf("StartupTask.RequestEnableAsync: %w", err)
		}
		var st int32 // StartupTaskState is an int32 enum, not an interface
		if hr := op.call(idxAsyncOpGetResults, uintptr(unsafe.Pointer(&st))); failed(hr) {
			return 0, fmt.Errorf("StartupTask.RequestEnableAsync results: %w", hresult(hr))
		}
		return startupState(st), nil
	})
}

// startupTaskDisable turns the task off and returns the state afterwards, so
// the caller can tell when a policy keeps it on regardless.
func startupTaskDisable() (startupState, error) {
	return winrtCall(func() (startupState, error) {
		task, err := getStartupTask()
		if err != nil {
			return 0, err
		}
		defer task.release()
		if hr := task.call(idxDisable); failed(hr) {
			return 0, fmt.Errorf("StartupTask.Disable: %w", hresult(hr))
		}
		st, err := readState(task)
		if err != nil {
			return stateDisabled, nil // Disable itself succeeded; that is the answer
		}
		return st, nil
	})
}

// getStartupTask resolves this package's startup task by its manifest id.
// Must run on the WinRT thread.
func getStartupTask() (comObj, error) {
	statics, err := factory(classStartupTask, iidStartupTaskStatics)
	if err != nil {
		return comObj{}, err
	}
	defer statics.release()

	id, err := newHStr(startupTaskID)
	if err != nil {
		return comObj{}, err
	}
	defer id.free()

	var raw unsafe.Pointer
	if hr := statics.call(idxGetAsync, uintptr(id.h), uintptr(unsafe.Pointer(&raw))); failed(hr) {
		return comObj{}, fmt.Errorf("StartupTask.GetAsync(%s): %w", startupTaskID, hresult(hr))
	}
	if raw == nil {
		return comObj{}, fmt.Errorf("StartupTask.GetAsync(%s) returned no operation", startupTaskID)
	}
	opAny := comObj{p: raw}
	defer opAny.release()
	// GetAsync is declared to return exactly this interface; the QI makes that
	// an assertion, so a mismatch fails here instead of calling a wrong slot.
	op, err := opAny.queryInterface(iidAsyncOpStartupTask)
	if err != nil {
		return comObj{}, err
	}
	defer op.release()
	if err := waitAsync(op); err != nil {
		return comObj{}, fmt.Errorf("StartupTask.GetAsync(%s): %w", startupTaskID, err)
	}

	var res unsafe.Pointer
	if hr := op.call(idxAsyncOpGetResults, uintptr(unsafe.Pointer(&res))); failed(hr) {
		return comObj{}, fmt.Errorf("StartupTask.GetAsync(%s) results: %w", startupTaskID, hresult(hr))
	}
	if res == nil {
		return comObj{}, fmt.Errorf("no startup task %q in this package's manifest", startupTaskID)
	}
	obj := comObj{p: res}
	defer obj.release()
	// The result is StartupTask's default interface, IStartupTask; as above,
	// the QI turns that into a checked fact.
	return obj.queryInterface(iidStartupTask)
}

func readState(task comObj) (startupState, error) {
	var st int32
	if hr := task.call(idxGetState, uintptr(unsafe.Pointer(&st))); failed(hr) {
		return 0, fmt.Errorf("StartupTask.State: %w", hresult(hr))
	}
	return startupState(st), nil
}

// waitAsync blocks until an async operation leaves the Started state, or the
// deadline passes. Polling IAsyncInfo.Status mirrors cfapi: the alternative is
// a completion-handler COM object synthesised in Go, with callbacks on an
// arbitrary thread, which is a bigger risk than a poll for calls this short.
func waitAsync(op comObj) error {
	info, err := op.queryInterface(iidAsyncInfo)
	if err != nil {
		return err
	}
	defer info.release()

	deadline := time.Now().Add(callTimeout)
	for {
		var status int32
		if hr := info.call(idxAsyncInfoStatus, uintptr(unsafe.Pointer(&status))); failed(hr) {
			return fmt.Errorf("IAsyncInfo.Status: %w", hresult(hr))
		}
		switch status {
		case asyncCompleted:
			return nil
		case asyncStarted:
			if time.Now().After(deadline) {
				info.call(idxAsyncInfoCancel)
				return errTimedOut
			}
			time.Sleep(5 * time.Millisecond)
		default: // Canceled or Error
			var code uint32
			info.call(idxAsyncInfoErrorCode, uintptr(unsafe.Pointer(&code)))
			info.call(idxAsyncInfoClose)
			return fmt.Errorf("async operation ended with status %d: %w", status, hresult(code))
		}
	}
}

// --- minimal COM plumbing (mirrors internal/cfapi) ---

// comObj is a raw WinRT interface pointer, held as an unsafe.Pointer so no
// uintptr-to-pointer conversion is ever needed: out-parameters are declared as
// unsafe.Pointer and the callee writes straight into them.
type comObj struct{ p unsafe.Pointer }

// call invokes vtable slot idx and returns the HRESULT (a 32-bit value; the
// upper half of the return register is not part of it).
//
// go:uintptrescapes makes every pointer converted to uintptr in a call's
// argument list escape to the heap and stay alive until the call returns.
// Without it a stack-allocated out-parameter could be moved by a stack growth
// between the conversion and the syscall, and Windows would write to the old
// address. x/sys/windows marks LazyProc.Call the same way.
//
//go:uintptrescapes
func (c comObj) call(idx int, args ...uintptr) uint32 {
	vtbl := *(**[64]uintptr)(c.p)
	all := make([]uintptr, 0, len(args)+1)
	all = append(all, uintptr(c.p))
	all = append(all, args...)
	hr, _, _ := syscall.SyscallN(vtbl[idx], all...)
	return uint32(hr)
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
	if failed(hr) {
		return comObj{}, fmt.Errorf("QueryInterface(%s): %w", iid, hresult(hr))
	}
	if out == nil {
		return comObj{}, fmt.Errorf("QueryInterface(%s) returned nothing", iid)
	}
	return comObj{p: out}, nil
}

// failed is the FAILED() macro: an HRESULT with the severity bit set.
func failed(hr uint32) bool { return int32(hr) < 0 }

func hresult(hr uint32) error { return ole.NewError(uintptr(hr)) }

var (
	combase                 = windows.NewLazySystemDLL("combase.dll")
	procWindowsCreateString = combase.NewProc("WindowsCreateString")
	procWindowsDeleteString = combase.NewProc("WindowsDeleteString")
)

// hstr is an owned HSTRING, sized in UTF-16 code units (go-ole's NewHString
// sizes in runes; see cfapi).
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
	if failed(uint32(hr)) {
		return hstr{}, hresult(uint32(hr))
	}
	return hstr{h: h}, nil
}

func (s hstr) free() {
	if s.h != 0 {
		_, _, _ = procWindowsDeleteString.Call(uintptr(s.h))
	}
}

// factory returns class's activation factory as the interface iid.
// RoGetActivationFactory queries for iid itself, so a wrong IID fails here.
func factory(class, iid string) (comObj, error) {
	insp, err := ole.RoGetActivationFactory(class, ole.NewGUID(iid))
	if err != nil {
		return comObj{}, fmt.Errorf("activation factory %s: %w", class, err)
	}
	return comObj{p: unsafe.Pointer(insp)}, nil
}

// --- the dedicated apartment thread ---
//
// COM apartment state is per thread and Go moves goroutines between threads,
// so every WinRT call runs on one locked thread initialised once with
// RoInitialize(MTA) and kept for the life of the process. Only packaged
// processes ever start it.

// winrtJob runs on the WinRT thread. initErr is non-nil when the thread could
// not initialise, in which case the job just reports it.
type winrtJob func(initErr error)

var (
	winrtOnce sync.Once
	winrtCh   chan winrtJob
)

const (
	roInitMultithreaded = 1
	rpcEChangedMode     = 0x80010106 // thread already in an STA: still usable
)

// winrtCall runs fn on the WinRT thread and returns its result, or errTimedOut
// if the thread does not take the job and finish it within callTimeout. A job
// that finishes after its caller gave up replies into a buffered channel that
// nobody reads, so the thread never blocks on it.
func winrtCall[T any](fn func() (T, error)) (T, error) {
	winrtOnce.Do(func() {
		winrtCh = make(chan winrtJob)
		go winrtLoop()
	})

	type result struct {
		v   T
		err error
	}
	reply := make(chan result, 1)
	job := func(initErr error) {
		if initErr != nil {
			reply <- result{err: initErr}
			return
		}
		v, err := fn()
		reply <- result{v: v, err: err}
	}

	var zero T
	timer := time.NewTimer(callTimeout)
	defer timer.Stop()
	select {
	case winrtCh <- job:
	case <-timer.C:
		return zero, errTimedOut // the thread is still busy with an earlier call
	}
	select {
	case r := <-reply:
		return r.v, r.err
	case <-timer.C:
		return zero, errTimedOut
	}
}

func winrtLoop() {
	runtime.LockOSThread() // never unlocked; this thread lives for the process
	var initErr error
	if err := ole.RoInitialize(roInitMultithreaded); err != nil {
		// go-ole reports every non-zero HRESULT as an error, including S_FALSE
		// ("already initialised"), which is success. RPC_E_CHANGED_MODE means
		// the thread already belongs to an STA, which still works for
		// StartupTask (an agile class). Anything else is fatal for WinRT.
		var oleErr *ole.OleError
		if !errors.As(err, &oleErr) || (failed(uint32(oleErr.Code())) && uint32(oleErr.Code()) != rpcEChangedMode) {
			initErr = fmt.Errorf("RoInitialize: %w", err)
		}
	}
	for job := range winrtCh {
		job(initErr)
	}
}
