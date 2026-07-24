//go:build windows

package main

// Toast activation: when a toast (or one of its buttons) is clicked, Windows calls
// our INotificationActivationCallback::Activate with the clicked element's
// arguments. We implement it as a classic in-process COM server registered with
// CoRegisterClassObject, following the proven pattern in
// git.sr.ht/~jackmordaunt/go-toast (which we can't import directly).
//
// It is registered from a dedicated thread held in the MULTITHREADED apartment for
// the process lifetime, so COM delivers Activate on its own RPC worker threads —
// no window-message pump is involved (unlike the earlier toast-raiser experiment
// that wedged the shell). The callback therefore runs on an arbitrary COM thread
// and must marshal any UI work to the UI thread itself.

import (
	"log/slog"
	"runtime"
	"syscall"
	"unsafe"

	ole "github.com/go-ole/go-ole"
	"golang.org/x/sys/windows"
)

// toastActivatorCLSID MUST match packaging/msix/AppxManifest.xml's
// ToastActivatorCLSID and com:Class Id. Permanent — part of the app's toast
// identity (see docs/specs/2026-07-22-actionable-toast-notifications.md).
var toastActivatorCLSID = ole.NewGUID("{00EEDCE7-5C4E-4573-85C2-98790F8F98AE}")

var (
	iidIClassFactory                   = ole.NewGUID("{00000001-0000-0000-C000-000000000046}")
	iidINotificationActivationCallback = ole.NewGUID("{53E31837-6600-4A81-9395-75CFFE746F94}")
)

// toastActivationHandler receives a clicked toast/button's invoked-args string.
// Set once before registerToastActivator; called on a COM RPC thread.
var toastActivationHandler func(args string)

type iClassFactory struct{ vtbl *iClassFactoryVtbl }

type iClassFactoryVtbl struct {
	ole.IUnknownVtbl
	CreateInstance uintptr
	LockServer     uintptr
}

type iNotificationActivationCallback struct{ vtbl *iNotificationActivationCallbackVtbl }

type iNotificationActivationCallbackVtbl struct {
	ole.IUnknownVtbl
	Activate uintptr
}

var (
	activatorObj    = &iNotificationActivationCallback{}
	factoryObj      = &iClassFactory{}
	activatorPinner runtime.Pinner
)

func init() {
	activatorObj.vtbl = &iNotificationActivationCallbackVtbl{
		IUnknownVtbl: ole.IUnknownVtbl{
			QueryInterface: syscall.NewCallback(func(this *iNotificationActivationCallback, riid *ole.GUID, out unsafe.Pointer) uintptr {
				if !ole.IsEqualGUID(riid, iidINotificationActivationCallback) && !ole.IsEqualGUID(riid, ole.IID_IUnknown) {
					return ole.E_NOINTERFACE
				}
				*(**iNotificationActivationCallback)(out) = this
				return ole.S_OK
			}),
			AddRef:  syscall.NewCallback(func(this *iNotificationActivationCallback) uintptr { return 1 }),
			Release: syscall.NewCallback(func(this *iNotificationActivationCallback) uintptr { return 1 }),
		},
		Activate: syscall.NewCallback(func(this, aumid, invokedArgs, data unsafe.Pointer, count uint32) uintptr {
			args := windows.UTF16PtrToString((*uint16)(invokedArgs))
			if h := toastActivationHandler; h != nil {
				h(args)
			}
			return ole.S_OK
		}),
	}
	factoryObj.vtbl = &iClassFactoryVtbl{
		IUnknownVtbl: ole.IUnknownVtbl{
			QueryInterface: syscall.NewCallback(func(this *iClassFactory, riid *ole.GUID, out unsafe.Pointer) uintptr {
				if !ole.IsEqualGUID(riid, iidIClassFactory) && !ole.IsEqualGUID(riid, ole.IID_IUnknown) {
					return ole.E_NOINTERFACE
				}
				*(**iClassFactory)(out) = this
				return ole.S_OK
			}),
			AddRef:  syscall.NewCallback(func(this *iClassFactory) uintptr { return 1 }),
			Release: syscall.NewCallback(func(this *iClassFactory) uintptr { return 1 }),
		},
		CreateInstance: syscall.NewCallback(func(this *iClassFactory, outer *ole.IUnknown, riid *ole.GUID, out unsafe.Pointer) uintptr {
			if outer != nil {
				return ole.E_NOINTERFACE // no aggregation (CLASS_E_NOAGGREGATION)
			}
			if !ole.IsEqualGUID(riid, iidINotificationActivationCallback) && !ole.IsEqualGUID(riid, ole.IID_IUnknown) {
				return ole.E_NOINTERFACE
			}
			*(**iNotificationActivationCallback)(out) = activatorObj
			return ole.S_OK
		}),
		LockServer: syscall.NewCallback(func(this *iClassFactory, lock uintptr) uintptr { return ole.S_OK }),
	}
	// COM holds only raw pointers to these; pin them so the GC never moves/frees them.
	activatorPinner.Pin(activatorObj)
	activatorPinner.Pin(activatorObj.vtbl)
	activatorPinner.Pin(factoryObj)
	activatorPinner.Pin(factoryObj.vtbl)
}

var (
	combaseDLL                = windows.NewLazySystemDLL("combase.dll")
	procCoRegisterClassObject = combaseDLL.NewProc("CoRegisterClassObject")
)

const (
	clsctxLocalServer   = 0x4
	regclsMultipleUse   = 0x1
	roInitMultithreaded = 1
)

// registerToastActivator makes clicked toasts/buttons call Activate. Best-effort:
// on failure it logs and toasts simply stay non-actionable (never fatal).
func registerToastActivator() {
	go func() {
		runtime.LockOSThread() // never released — keeps this MTA thread (and the apartment) alive
		if err := ole.RoInitialize(roInitMultithreaded); err != nil {
			slog.Warn("toast activator: RoInitialize failed", "err", err)
			// still attempt to register — CoRegisterClassObject will surface any real problem
		}
		var cookie uint32
		hr, _, _ := procCoRegisterClassObject.Call(
			uintptr(unsafe.Pointer(toastActivatorCLSID)),
			uintptr(unsafe.Pointer(factoryObj)),
			clsctxLocalServer,
			regclsMultipleUse,
			uintptr(unsafe.Pointer(&cookie)),
		)
		if hr != 0 {
			slog.Warn("toast activator registration failed; toast buttons won't route", "hr", uint32(hr))
			return
		}
		slog.Info("toast activator registered")
		select {} // hold the thread + MTA for the process lifetime
	}()
}
