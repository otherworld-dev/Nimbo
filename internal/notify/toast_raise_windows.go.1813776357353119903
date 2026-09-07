//go:build windows

package notify

import (
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	ole "github.com/go-ole/go-ole"
	"github.com/otherworld/nimbo/internal/brand"
	"golang.org/x/sys/windows"
)

// Native in-process WinRT toast raising — the way Windows itself raises toasts,
// with no child process and no PowerShell (and so no injection surface). We drive
// the same object graph the platform does:
//
//	RoActivateInstance("…XmlDocument") → IXmlDocumentIO::LoadXml(xml)
//	RoGetActivationFactory("…ToastNotification")   → CreateToastNotification(doc)
//	RoGetActivationFactory("…ToastNotificationManager") → GetDefault()
//	        → IToastNotificationManagerForUser::CreateToastNotifierWithId(AUMID)
//	        → IToastNotifier::Show(toast)
//
// go-ole supplies the plumbing (RoActivateInstance/RoGetActivationFactory,
// IUnknown/IInspectable, GUID/error helpers) — it is already in the module graph
// (Wails pulls it) and these exact bindings ship in production via beeep. The one
// go-ole function we do NOT use is NewHString: it sizes the HSTRING by rune count
// instead of UTF-16 code units, which truncates non-BMP text (emoji) — see
// newHString below. The interface IIDs and vtable slot orders are transcribed
// from the Windows SDK winmd and cross-checked against the winrt-go-gen output in
// git.sr.ht/~jackmordaunt/go-toast (which we cannot import — it lives under
// internal/ — but can verify against).

// Runtime-class names (ASCII → go-ole's internal NewHString is safe for these).
const (
	classXmlDocument           = "Windows.Data.Xml.Dom.XmlDocument"
	classToastNotification     = "Windows.UI.Notifications.ToastNotification"
	classToastNotificationMgr  = "Windows.UI.Notifications.ToastNotificationManager"
)

// Interface IIDs (verified against the winmd / winrt-go-gen bindings).
const (
	iidXmlDocumentIO                    = "6cd0e74e-ee65-4489-9ebf-ca43e87ba637"
	iidToastNotificationFactory         = "04124b20-82c6-4229-b109-fd9ed4662b53"
	iidToastNotificationManagerStatics5 = "d6f5f569-d40d-407c-8989-88cab42cfd14"
	iidToastNotificationManagerForUser  = "79ab57f6-43fe-487b-8a7f-99567200ae94"
	iidToastNotifier                    = "75927b93-03f3-41ec-91d3-6e5bac1b38e7"
)

const roInitMultithreaded = 1 // RO_INIT_MULTITHREADED

// Vtable layouts. Each embeds IInspectableVtbl (IUnknown's 3 slots + WinRT's 3),
// so the runtime-class methods that follow land on their true slot indices.

type xmlDocumentIOVtbl struct {
	ole.IInspectableVtbl
	LoadXml             uintptr
	LoadXmlWithSettings uintptr
	SaveToFileAsync     uintptr
}

type toastNotificationFactoryVtbl struct {
	ole.IInspectableVtbl
	CreateToastNotification uintptr
}

type toastNotificationManagerStatics5Vtbl struct {
	ole.IInspectableVtbl
	GetDefault uintptr
}

type toastNotificationManagerForUserVtbl struct {
	ole.IInspectableVtbl
	CreateToastNotifier       uintptr
	CreateToastNotifierWithId uintptr
	GetHistory                uintptr
	GetUser                   uintptr
}

type toastNotifierVtbl struct {
	ole.IInspectableVtbl
	Show                           uintptr
	Hide                           uintptr
	GetSetting                     uintptr
	AddToSchedule                  uintptr
	RemoveFromSchedule             uintptr
	GetScheduledToastNotifications uintptr
}

var (
	combase                 = windows.NewLazySystemDLL("combase.dll")
	procWindowsCreateString = combase.NewProc("WindowsCreateString")
	procWindowsDeleteString = combase.NewProc("WindowsDeleteString")

	kernel32                        = windows.NewLazySystemDLL("kernel32.dll")
	procGetCurrentPackageFamilyName = kernel32.NewProc("GetCurrentPackageFamilyName")
)

// newHString creates an HSTRING sized by UTF-16 **code units**, so non-BMP
// characters (emoji, etc.) in server-supplied text are preserved. This is the one
// thing go-ole's NewHString gets wrong (it uses the rune count).
func newHString(s string) (ole.HString, error) {
	u16 := utf16.Encode([]rune(s))
	u16 = append(u16, 0) // NUL terminator; length below excludes it
	var h ole.HString
	hr, _, _ := procWindowsCreateString.Call(
		uintptr(unsafe.Pointer(&u16[0])),
		uintptr(len(u16)-1),
		uintptr(unsafe.Pointer(&h)),
	)
	runtime.KeepAlive(u16)
	if hr != 0 {
		return 0, ole.NewError(hr)
	}
	return h, nil
}

func deleteHString(h ole.HString) {
	if h != 0 {
		_, _, _ = procWindowsDeleteString.Call(uintptr(h))
	}
}

// packageFamilyName returns this process's MSIX package family name, or "" when
// unpackaged. (Duplicated from cmd/nimbo-gui/restart_windows.go for now; the spec
// calls for consolidating it into a shared location.)
func packageFamilyName() string {
	var length uint32
	procGetCurrentPackageFamilyName.Call(uintptr(unsafe.Pointer(&length)), 0)
	if length == 0 {
		return ""
	}
	buf := make([]uint16, length)
	r, _, _ := procGetCurrentPackageFamilyName.Call(uintptr(unsafe.Pointer(&length)), uintptr(unsafe.Pointer(&buf[0])))
	if r != 0 {
		return ""
	}
	return windows.UTF16ToString(buf)
}

// toastAUMID is the app user model id a packaged toast is bound to: the package
// family name plus the manifest Application id. Empty when unpackaged.
func toastAUMID() string {
	pfn := packageFamilyName()
	if pfn == "" {
		return ""
	}
	return pfn + "!" + brand.Current.AppID
}

// raiseNativeToast builds the toast XML and shows it in-process via WinRT.
func raiseNativeToast(title, message, link string) error {
	return raiseNativeXML(buildToastXML(title, message, link))
}

// toastGap staggers a burst of toasts so they cascade on screen instead of
// stacking on top of each other (the native path raises far faster than the old
// per-toast PowerShell spawn, which incidentally spaced them out).
const toastGap = 700 * time.Millisecond

type raiseReq struct{ xml, aumid string }

var (
	raiserOnce sync.Once
	raiserCh   chan raiseReq
)

// raiserLoop owns one OS thread for the process lifetime — initialised once into
// the multithreaded apartment (never the Wails STA UI thread) — and shows every
// toast from it, one at a time with a small gap. Serialising through a single
// thread also avoids the per-toast RoInitialize churn of the old model.
func raiserLoop() {
	runtime.LockOSThread() // intentionally never unlocked; the thread lives for the process
	if err := ole.RoInitialize(roInitMultithreaded); err != nil {
		slog.Error("toast: RoInitialize failed; native toasts disabled", "err", err)
		for range raiserCh { // keep draining so senders never block
		}
		return
	}
	for req := range raiserCh {
		if err := showToastXML(req.xml, req.aumid); err != nil {
			slog.Warn("native toast raise failed", "err", err)
		}
		time.Sleep(toastGap)
	}
}

// raiseNativeXML queues a fully-built toast for the shared raiser thread and
// returns IMMEDIATELY (never blocks the caller — toasts can be raised from the UI
// thread). Returns an error only when unpackaged (no AUMID) so the caller can
// fall back to the go-toast path; a queued toast reports success.
func raiseNativeXML(xml string) error {
	aumid := toastAUMID()
	if aumid == "" {
		return errors.New("not a packaged install")
	}
	raiserOnce.Do(func() {
		raiserCh = make(chan raiseReq, 32)
		go raiserLoop()
	})
	select {
	case raiserCh <- raiseReq{xml: xml, aumid: aumid}:
	default:
		slog.Warn("toast: raiser queue full, dropping toast")
	}
	return nil
}

// showToastXML performs the WinRT call sequence. Runs on the locked MTA thread.
func showToastXML(xml, aumid string) error {
	// XmlDocument + LoadXml.
	docInsp, err := ole.RoActivateInstance(classXmlDocument)
	if err != nil {
		return fmt.Errorf("activate XmlDocument: %w", err)
	}
	defer docInsp.Release()

	docIO, err := docInsp.QueryInterface(ole.NewGUID(iidXmlDocumentIO))
	if err != nil {
		return fmt.Errorf("QI IXmlDocumentIO: %w", err)
	}
	defer docIO.Release()
	xmlH, err := newHString(xml)
	if err != nil {
		return fmt.Errorf("HSTRING(xml): %w", err)
	}
	defer deleteHString(xmlH)
	if hr, _, _ := syscall.SyscallN(
		(*xmlDocumentIOVtbl)(unsafe.Pointer(docIO.RawVTable)).LoadXml,
		uintptr(unsafe.Pointer(docIO)),
		uintptr(xmlH),
	); hr != 0 {
		return fmt.Errorf("LoadXml: %w", ole.NewError(hr))
	}

	// ToastNotification factory → CreateToastNotification(doc).
	facInsp, err := ole.RoGetActivationFactory(classToastNotification, ole.NewGUID(iidToastNotificationFactory))
	if err != nil {
		return fmt.Errorf("factory ToastNotification: %w", err)
	}
	defer facInsp.Release()
	var toast *ole.IInspectable
	if hr, _, _ := syscall.SyscallN(
		(*toastNotificationFactoryVtbl)(unsafe.Pointer(facInsp.RawVTable)).CreateToastNotification,
		0, // static
		uintptr(unsafe.Pointer(docInsp)),
		uintptr(unsafe.Pointer(&toast)),
	); hr != 0 {
		return fmt.Errorf("CreateToastNotification: %w", ole.NewError(hr))
	}
	defer toast.Release()

	// ToastNotificationManager statics → GetDefault() → ForUser.
	mgrInsp, err := ole.RoGetActivationFactory(classToastNotificationMgr, ole.NewGUID(iidToastNotificationManagerStatics5))
	if err != nil {
		return fmt.Errorf("factory ToastNotificationManager: %w", err)
	}
	defer mgrInsp.Release()
	var forUser *ole.IInspectable
	if hr, _, _ := syscall.SyscallN(
		(*toastNotificationManagerStatics5Vtbl)(unsafe.Pointer(mgrInsp.RawVTable)).GetDefault,
		0, // static
		uintptr(unsafe.Pointer(&forUser)),
	); hr != 0 {
		return fmt.Errorf("GetDefault: %w", ole.NewError(hr))
	}
	defer forUser.Release()

	forUserItf, err := forUser.QueryInterface(ole.NewGUID(iidToastNotificationManagerForUser))
	if err != nil {
		return fmt.Errorf("QI IToastNotificationManagerForUser: %w", err)
	}
	defer forUserItf.Release()
	aumidH, err := newHString(aumid)
	if err != nil {
		return fmt.Errorf("HSTRING(aumid): %w", err)
	}
	defer deleteHString(aumidH)
	var notifier *ole.IInspectable
	if hr, _, _ := syscall.SyscallN(
		(*toastNotificationManagerForUserVtbl)(unsafe.Pointer(forUserItf.RawVTable)).CreateToastNotifierWithId,
		uintptr(unsafe.Pointer(forUserItf)),
		uintptr(aumidH),
		uintptr(unsafe.Pointer(&notifier)),
	); hr != 0 {
		return fmt.Errorf("CreateToastNotifierWithId: %w", ole.NewError(hr))
	}
	defer notifier.Release()

	// IToastNotifier::Show(toast).
	notifierItf, err := notifier.QueryInterface(ole.NewGUID(iidToastNotifier))
	if err != nil {
		return fmt.Errorf("QI IToastNotifier: %w", err)
	}
	defer notifierItf.Release()
	if hr, _, _ := syscall.SyscallN(
		(*toastNotifierVtbl)(unsafe.Pointer(notifierItf.RawVTable)).Show,
		uintptr(unsafe.Pointer(notifierItf)),
		uintptr(unsafe.Pointer(toast)),
	); hr != 0 {
		return fmt.Errorf("Show: %w", ole.NewError(hr))
	}
	// Note: Show can return S_OK yet suppress the toast (AUMID miss, notifications
	// off), so success here is "sent", not "shown".
	runtime.KeepAlive(docInsp)
	return nil
}

// buildToastXML assembles a ToastGeneric payload, XML-escaping every interpolated
// value. There is no script layer, so escaping only guards XmlDocument.LoadXml
// well-formedness.
func buildToastXML(title, message, link string) string {
	var b strings.Builder
	b.WriteString(`<toast`)
	if link != "" {
		b.WriteString(` activationType="protocol" launch="`)
		b.WriteString(xmlEscapeAttr(link))
		b.WriteString(`"`)
	}
	b.WriteString(`><visual><binding template="ToastGeneric">`)
	if title != "" {
		b.WriteString(`<text>`)
		b.WriteString(xmlEscapeText(title))
		b.WriteString(`</text>`)
	}
	if message != "" {
		b.WriteString(`<text>`)
		b.WriteString(xmlEscapeText(message))
		b.WriteString(`</text>`)
	}
	b.WriteString(`</binding></visual></toast>`)
	return b.String()
}

// buildToastXMLActions builds a toast whose body and buttons activate the app
// (foreground activation → our COM activator → dispatchToastActivation). bodyArgs
// is the activation string for a click on the toast body; each button carries its
// own args.
func buildToastXMLActions(title, message, bodyArgs string, buttons []ToastButton) string {
	var b strings.Builder
	b.WriteString(`<toast activationType="foreground" launch="`)
	b.WriteString(xmlEscapeAttr(bodyArgs))
	b.WriteString(`"><visual><binding template="ToastGeneric">`)
	if title != "" {
		b.WriteString(`<text>`)
		b.WriteString(xmlEscapeText(title))
		b.WriteString(`</text>`)
	}
	if message != "" {
		b.WriteString(`<text>`)
		b.WriteString(xmlEscapeText(message))
		b.WriteString(`</text>`)
	}
	b.WriteString(`</binding></visual>`)
	if len(buttons) > 0 {
		b.WriteString(`<actions>`)
		for _, btn := range buttons {
			b.WriteString(`<action activationType="foreground" content="`)
			b.WriteString(xmlEscapeAttr(btn.Label))
			b.WriteString(`" arguments="`)
			b.WriteString(xmlEscapeAttr(btn.Args))
			b.WriteString(`"/>`)
		}
		b.WriteString(`</actions>`)
	}
	b.WriteString(`</toast>`)
	return b.String()
}

// RaiseActionable shows a toast whose body and buttons route into the app when
// clicked. Native in-process on a packaged install; on an unpackaged/dev build
// (no AUMID) it degrades to a plain buttonless toast.
func RaiseActionable(title, message, bodyArgs string, buttons []ToastButton) {
	if !enabled.Load() {
		return
	}
	if title == "" {
		title = "Nimbo"
	}
	for i := range buttons {
		buttons[i].Label = sanitizeToastText(buttons[i].Label)
	}
	xml := buildToastXMLActions(title, message, bodyArgs, buttons)
	if err := raiseNativeXML(xml); err != nil {
		Toast(title, message, "") // unpackaged/dev fallback (no buttons)
	}
}

func xmlEscapeText(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func xmlEscapeAttr(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}
