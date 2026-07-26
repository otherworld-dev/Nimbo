//go:build windows

package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Repairing shortcuts whose icon path died with the old app identity — see the
// header of appshortcuts.go for how they got that way. Start-menu entries and
// taskbar pins are both swept: the Start entry would eventually rewrite itself
// on the next window open, but a pin is Windows' own copy of the .lnk and
// nothing else will ever touch it.
//
// Reading a .lnk is the same raw-COM dance as writing one (shortcut_windows.go)
// with IPersistFile::Load in front. Only the icon is changed; the target,
// arguments and the AppUserModelID property are left exactly as loaded, so a
// pin keeps the identity Windows has already filed it under.

var (
	procSHChangeNotify = shell32.NewProc("SHChangeNotify")

	// repairMu serialises sweeps: startup and a window open can both trigger
	// one, and two threads rewriting the same .lnk is worth avoiding.
	repairMu sync.Mutex
)

const (
	shcneUpdateItem   = 0x00002000
	shcneAssocChanged = 0x08000000
	shcnfIDList       = 0x0000
	shcnfPathW        = 0x0005
	shcnfFlush        = 0x1000

	// SLGP_RAWPATH — the path as stored, with no attempt to resolve or relocate
	// it. The target is an AppExecutionAlias shim; letting the shell "help"
	// could rewrite it.
	slgpRawPath = 0x4

	stgmRead = 0x0
)

// pinnedTaskbarDir is where Explorer keeps its private copies of the shortcuts
// the user has pinned to the taskbar. Undocumented but unchanged since Windows
// 7, and the only way to reach a pin's icon: there is no API for it.
func pinnedTaskbarDir() string {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		return ""
	}
	return filepath.Join(appData, "Microsoft", "Internet Explorer", "Quick Launch", "User Pinned", "TaskBar")
}

// hidePath sets the hidden attribute, so the icon folder doesn't surface as an
// entry in the Start menu. Best-effort: a visible folder is untidy, not broken.
func hidePath(p string) {
	u, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return
	}
	attrs, err := windows.GetFileAttributes(u)
	if err != nil || attrs&windows.FILE_ATTRIBUTE_HIDDEN != 0 {
		return
	}
	_ = windows.SetFileAttributes(u, attrs|windows.FILE_ATTRIBUTE_HIDDEN)
}

// shellLink is a loaded .lnk: the IShellLinkW plus the IPersistFile it came
// from, so it can be saved back.
type shellLink struct {
	link *iShellLinkW
	pf   *iPersistFile
}

func (l *shellLink) release() {
	if l.pf != nil {
		comRelease(unsafe.Pointer(l.pf), l.pf.lpVtbl.Release)
	}
	if l.link != nil {
		comRelease(unsafe.Pointer(l.link), l.link.lpVtbl.Release)
	}
}

// loadShellLink opens an existing .lnk for inspection. The caller must be on a
// thread with COM already initialised (see repairAppShortcutIcons).
func loadShellLink(path string) (*shellLink, error) {
	var link *iShellLinkW
	const clsctxInproc = 0x1
	r, _, _ := procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(&clsidShellLink)), 0, clsctxInproc,
		uintptr(unsafe.Pointer(&iidIShellLinkW)), uintptr(unsafe.Pointer(&link)))
	if r != 0 || link == nil {
		return nil, windows.Errno(r)
	}
	l := &shellLink{link: link}
	if err := hrCall(link.lpVtbl.QueryInterface, uintptr(unsafe.Pointer(link)),
		uintptr(unsafe.Pointer(&iidIPersistFile)), uintptr(unsafe.Pointer(&l.pf))); err != nil {
		l.release()
		return nil, err
	}
	p, err := utf16Arg(path)
	if err != nil {
		l.release()
		return nil, err
	}
	if err := hrCall(l.pf.lpVtbl.Load, uintptr(unsafe.Pointer(l.pf)), p, stgmRead); err != nil {
		l.release()
		return nil, err
	}
	return l, nil
}

// target is the shortcut's stored launch path.
func (l *shellLink) target() string {
	buf := make([]uint16, windows.MAX_LONG_PATH)
	var fd windows.Win32finddata
	r, _, _ := syscall.SyscallN(l.link.lpVtbl.GetPath, uintptr(unsafe.Pointer(l.link)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)),
		uintptr(unsafe.Pointer(&fd)), slgpRawPath)
	if r != 0 { // S_FALSE too: a link with no path is not one of ours
		return ""
	}
	return windows.UTF16ToString(buf)
}

// arguments is the shortcut's stored command line.
func (l *shellLink) arguments() string {
	buf := make([]uint16, 4096)
	r, _, _ := syscall.SyscallN(l.link.lpVtbl.GetArguments, uintptr(unsafe.Pointer(l.link)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if r != 0 {
		return ""
	}
	return windows.UTF16ToString(buf)
}

// iconLocation is the shortcut's stored icon path, as written (it may still
// hold environment variables, so compare it as a string, not as a real path).
func (l *shellLink) iconLocation() string {
	buf := make([]uint16, windows.MAX_LONG_PATH)
	var index int32
	r, _, _ := syscall.SyscallN(l.link.lpVtbl.GetIconLocation, uintptr(unsafe.Pointer(l.link)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unsafe.Pointer(&index)))
	if r != 0 {
		return ""
	}
	return windows.UTF16ToString(buf)
}

// setIconAndSave points the shortcut at a new icon and writes it back in place.
func (l *shellLink) setIconAndSave(path, icoPath string) error {
	ip, err := utf16Arg(icoPath)
	if err != nil {
		return err
	}
	if err := hrCall(l.link.lpVtbl.SetIconLocation, uintptr(unsafe.Pointer(l.link)), ip, 0); err != nil {
		return err
	}
	p, err := utf16Arg(path)
	if err != nil {
		return err
	}
	return hrCall(l.pf.lpVtbl.Save, uintptr(unsafe.Pointer(l.pf)), p, 1)
}

// repairAppShortcutIcons re-points every app shortcut of ours — Start-menu
// entry and taskbar pin alike — whose icon path no longer matches where the
// icons actually live. Safe to call repeatedly: a shortcut that already carries
// the right path is left untouched, so a sweep with nothing to do costs a
// handful of file reads.
func (a *App) repairAppShortcutIcons() {
	repairMu.Lock()
	defer repairMu.Unlock()

	dir := appIconsDir()
	if dir == "" {
		return
	}
	// Carry existing icons over from the old package-bound cache first, so an
	// install that already has them can be repaired offline, in this one pass,
	// rather than waiting on a signed-in engine to re-fetch every icon.
	if n := migrateLegacyAppIcons(legacyAppIconsDir(), dir); n > 0 {
		slog.Info("moved app icons out of the package data root", "count", n, "dir", dir)
	}

	selfExe, _ := os.Executable()
	alias := launcherAlias()

	// COM wants a consistent apartment; hold the thread for the whole sweep.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	const coinitApartment = 0x2
	r, _, _ := procCoInitializeEx.Call(0, coinitApartment)
	if r != 0 && r != 1 { // S_OK or S_FALSE (already initialised)
		return
	}
	defer procCoUninitialize.Call()

	fixed := 0
	for _, d := range []string{shortcutsDir(), pinnedTaskbarDir()} {
		if d == "" {
			continue
		}
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".lnk") {
				continue
			}
			if a.repairShortcutIcon(filepath.Join(d, e.Name()), alias, selfExe) {
				fixed++
			}
		}
	}
	if fixed > 0 {
		// Explorer caches shortcut icons hard; nudge it to re-read them. A
		// taskbar pin may still repaint only after Explorer next restarts, but
		// the file on disk is correct from here on.
		procSHChangeNotify.Call(shcneAssocChanged, shcnfIDList|shcnfFlush, 0, 0)
		slog.Info("repaired app shortcut icons", "count", fixed)
	}
}

// repairShortcutIcon fixes one .lnk if it is ours and its icon path is wrong.
// Reports whether it changed anything.
func (a *App) repairShortcutIcon(lnkPath, alias, selfExe string) bool {
	l, err := loadShellLink(lnkPath)
	if err != nil {
		return false
	}
	defer l.release()

	if !isOurShortcutTarget(l.target(), alias, selfExe) {
		return false
	}
	id, ok := appIDFromShortcutArgs(l.arguments())
	if !ok {
		return false
	}
	want := a.appIconPath(id)
	if want == "" || strings.EqualFold(l.iconLocation(), want) {
		return false // not ours to fix, or already right
	}
	if _, err := os.Stat(want); err != nil {
		// Nothing at the new path yet — generate it rather than swap one dead
		// path for another. A no-op while signed out; the next sweep retries.
		a.ensureAppIcon(id, true)
		if _, err := os.Stat(want); err != nil {
			return false
		}
	}
	if err := l.setIconAndSave(lnkPath, want); err != nil {
		slog.Debug("could not repair shortcut icon", "lnk", filepath.Base(lnkPath), "err", err)
		return false
	}
	if p, err := windows.UTF16PtrFromString(lnkPath); err == nil {
		procSHChangeNotify.Call(shcneUpdateItem, shcnfPathW|shcnfFlush, uintptr(unsafe.Pointer(p)), 0)
	}
	slog.Info("repaired app shortcut icon", "lnk", filepath.Base(lnkPath), "app", id, "icon", want)
	return true
}
