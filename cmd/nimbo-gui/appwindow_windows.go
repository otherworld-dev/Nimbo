//go:build windows

package main

import (
	"log/slog"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Per-window taskbar identity. Windows groups taskbar buttons by a window's
// AppUserModelID; setting a distinct AUMID on an app window's property store
// (before it is first shown) gives it its own taskbar slot, and WM_SETICON
// gives that slot the app's icon. Wails doesn't expose either, so this talks
// to shell32/user32 directly, in the same lazy-DLL style as restart_windows.go.

var (
	shell32                         = windows.NewLazySystemDLL("shell32.dll")
	procSHGetPropertyStoreForWindow = shell32.NewProc("SHGetPropertyStoreForWindow")
	user32w                         = windows.NewLazySystemDLL("user32.dll")
	procSendMessageW                = user32w.NewProc("SendMessageW")
	procLoadImageW                  = user32w.NewProc("LoadImageW")
	procGetSystemMetrics            = user32w.NewProc("GetSystemMetrics")
)

// propertyKey mirrors PROPERTYKEY.
type propertyKey struct {
	fmtid windows.GUID
	pid   uint32
}

// PKEY_AppUserModel_ID = {9F4C2855-9F79-4B39-A8D0-E1D42DE1D5F3}, 5
var pkeyAppUserModelID = propertyKey{
	fmtid: windows.GUID{Data1: 0x9F4C2855, Data2: 0x9F79, Data3: 0x4B39,
		Data4: [8]byte{0xA8, 0xD0, 0xE1, 0xD4, 0x2D, 0xE1, 0xD5, 0xF3}},
	pid: 5,
}

// iidIPropertyStore = {886D8EEB-8CF2-4446-8D02-CDBA1DBDCF99}
var iidIPropertyStore = windows.GUID{Data1: 0x886D8EEB, Data2: 0x8CF2, Data3: 0x4446,
	Data4: [8]byte{0x8D, 0x02, 0xCD, 0xBA, 0x1D, 0xBD, 0xCF, 0x99}}

// propVariant mirrors PROPVARIANT for VT_LPWSTR (large enough for all uses).
type propVariant struct {
	vt       uint16
	_        [6]byte
	pwszVal  *uint16
	_        [8]byte
}

const vtLPWSTR = 31

// iPropertyStore is the IPropertyStore COM vtable.
type iPropertyStore struct{ lpVtbl *iPropertyStoreVtbl }
type iPropertyStoreVtbl struct {
	QueryInterface uintptr
	AddRef         uintptr
	Release        uintptr
	GetCount       uintptr
	GetAt          uintptr
	GetValue       uintptr
	SetValue       uintptr
	Commit         uintptr
}

func (ps *iPropertyStore) SetValue(key *propertyKey, pv *propVariant) error {
	r, _, _ := syscall.SyscallN(ps.lpVtbl.SetValue, uintptr(unsafe.Pointer(ps)),
		uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(pv)))
	if r != 0 {
		return windows.Errno(r)
	}
	return nil
}
func (ps *iPropertyStore) Commit() error {
	r, _, _ := syscall.SyscallN(ps.lpVtbl.Commit, uintptr(unsafe.Pointer(ps)))
	if r != 0 {
		return windows.Errno(r)
	}
	return nil
}
func (ps *iPropertyStore) Release() {
	syscall.SyscallN(ps.lpVtbl.Release, uintptr(unsafe.Pointer(ps)))
}

// setWindowAUMID stamps an AppUserModelID onto a window's property store.
func setWindowAUMID(hwnd uintptr, aumid string) error {
	var ps *iPropertyStore
	r, _, _ := procSHGetPropertyStoreForWindow.Call(hwnd,
		uintptr(unsafe.Pointer(&iidIPropertyStore)), uintptr(unsafe.Pointer(&ps)))
	if r != 0 || ps == nil {
		return windows.Errno(r)
	}
	defer ps.Release()

	w, err := windows.UTF16PtrFromString(aumid)
	if err != nil {
		return err
	}
	// VT_LPWSTR PROPVARIANT pointing at our Go-managed UTF-16 — safe for the
	// synchronous SetValue+Commit (the store copies the value), and we clear
	// nothing afterwards since the memory is Go's.
	pv := propVariant{vt: vtLPWSTR, pwszVal: w}
	if err := ps.SetValue(&pkeyAppUserModelID, &pv); err != nil {
		return err
	}
	return ps.Commit()
}

// Window-message constants for icons.
const (
	wmSetIcon    = 0x0080
	iconSmall    = 0
	iconBig      = 1
	imageIcon    = 1
	lrLoadFromFile = 0x0010
	smCXIcon     = 11
	smCXSMIcon   = 49
)

// appIconCache holds the loaded HICON pair per app id for the process
// lifetime. LoadImageW(LR_LOADFROMFILE) hands us ownership and WM_SETICON does
// NOT take it — without the cache every window open/close cycle would leak two
// USER handles (10k/process cap). Caching bounds it to 2 per distinct app and
// makes reopens cheaper. Only touched on the UI thread (via InvokeSync), so no
// lock. Trade-off: an icon refreshed on disk shows after the next app restart.
var appIconCache = map[string][2]uintptr{}

// setWindowIcon applies the app's .ico as the window's big and small icons
// (taskbar + title bar), loading it once per app id. Missing/invalid file →
// silently skipped; the window keeps the app-wide brand icon.
func setWindowIcon(hwnd uintptr, id, icoPath string) {
	hs, ok := appIconCache[id]
	if !ok {
		p, err := windows.UTF16PtrFromString(icoPath)
		if err != nil {
			return
		}
		load := func(metric uintptr) uintptr {
			cx, _, _ := procGetSystemMetrics.Call(metric)
			h, _, _ := procLoadImageW.Call(0, uintptr(unsafe.Pointer(p)), imageIcon, cx, cx, lrLoadFromFile)
			return h
		}
		hs = [2]uintptr{load(smCXIcon), load(smCXSMIcon)}
		if hs[0] == 0 && hs[1] == 0 {
			return // don't cache a failed load (file may appear later)
		}
		appIconCache[id] = hs
	}
	if hs[0] != 0 {
		procSendMessageW.Call(hwnd, wmSetIcon, iconBig, hs[0])
	}
	if hs[1] != 0 {
		procSendMessageW.Call(hwnd, wmSetIcon, iconSmall, hs[1])
	}
}

// setAppWindowIdentity applies the per-app taskbar identity to a native window:
// its own AUMID (own taskbar slot) and its own icon. Call on the UI thread
// before the window is first shown.
func setAppWindowIdentity(hwnd unsafe.Pointer, id, aumid, icoPath string) {
	if hwnd == nil {
		return
	}
	if err := setWindowAUMID(uintptr(hwnd), aumid); err != nil {
		slog.Warn("could not set app window AUMID", "aumid", aumid, "err", err)
	}
	setWindowIcon(uintptr(hwnd), id, icoPath)
}

// refreshWindowIcon re-applies a window's icon after the .ico was (re)generated
// — the window may have opened before the icon existed (first open, or an
// icon-style migration) and would otherwise keep the brand fallback until the
// next app restart. Drops the cached HICONs first so a stale pre-migration
// icon can't be re-served. UI thread only.
func refreshWindowIcon(hwnd unsafe.Pointer, id, icoPath string) {
	if hwnd == nil {
		return
	}
	delete(appIconCache, id) // force a reload from the (possibly regenerated) file
	setWindowIcon(uintptr(hwnd), id, icoPath)
}
