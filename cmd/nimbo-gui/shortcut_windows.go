//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/otherworld/nimbo/internal/brand"
)

// Start-menu shortcuts for app windows. A .lnk under
// %APPDATA%\Microsoft\Windows\Start Menu\Programs\<Brand> Apps\ makes a
// Nextcloud app launchable/pinnable like any desktop app. Two details matter:
//   - The target must be the package's AppExecutionAlias
//     (%LOCALAPPDATA%\Microsoft\WindowsApps\<alias>.exe), NOT the exe under
//     WindowsApps — that path changes on every update and would strand pins.
//   - The .lnk must carry the SAME System.AppUserModel.ID as the app's window,
//     so the pinned button and the running window share one taskbar slot and
//     the pin can relaunch the app.
// Written via raw COM (IShellLinkW + IPropertyStore + IPersistFile) in the
// same lazy-DLL style as the rest of the package — no new dependencies.

var (
	ole32                = windows.NewLazySystemDLL("ole32.dll")
	procCoInitializeEx   = ole32.NewProc("CoInitializeEx")
	procCoUninitialize   = ole32.NewProc("CoUninitialize")
	procCoCreateInstance = ole32.NewProc("CoCreateInstance")
)

var (
	clsidShellLink  = windows.GUID{Data1: 0x00021401, Data4: [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidIShellLinkW  = windows.GUID{Data1: 0x000214F9, Data4: [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidIPersistFile = windows.GUID{Data1: 0x0000010B, Data4: [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
)

type iShellLinkW struct{ lpVtbl *iShellLinkWVtbl }
type iShellLinkWVtbl struct {
	QueryInterface      uintptr
	AddRef              uintptr
	Release             uintptr
	GetPath             uintptr
	GetIDList           uintptr
	SetIDList           uintptr
	GetDescription      uintptr
	SetDescription      uintptr
	GetWorkingDirectory uintptr
	SetWorkingDirectory uintptr
	GetArguments        uintptr
	SetArguments        uintptr
	GetHotkey           uintptr
	SetHotkey           uintptr
	GetShowCmd          uintptr
	SetShowCmd          uintptr
	GetIconLocation     uintptr
	SetIconLocation     uintptr
	SetRelativePath     uintptr
	Resolve             uintptr
	SetPath             uintptr
}

type iPersistFile struct{ lpVtbl *iPersistFileVtbl }
type iPersistFileVtbl struct {
	QueryInterface uintptr
	AddRef         uintptr
	Release        uintptr
	GetClassID     uintptr
	IsDirty        uintptr
	Load           uintptr
	Save           uintptr
	SaveCompleted  uintptr
	GetCurFile     uintptr
}

func hrCall(trap uintptr, args ...uintptr) error {
	r, _, _ := syscall.SyscallN(trap, args...)
	if r != 0 {
		return windows.Errno(r)
	}
	return nil
}

func comRelease(obj unsafe.Pointer, releaseSlot uintptr) {
	syscall.SyscallN(releaseSlot, uintptr(obj))
}

func utf16Arg(s string) (uintptr, error) {
	p, err := windows.UTF16PtrFromString(s)
	if err != nil {
		return 0, err
	}
	return uintptr(unsafe.Pointer(p)), nil
}

// shortcutsDir is the Start-menu folder holding the per-app shortcuts.
func shortcutsDir() string {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		return ""
	}
	return filepath.Join(appData, "Microsoft", "Windows", "Start Menu", "Programs", brand.Current.Name+" Apps")
}

// shortcutExists reports whether a .lnk with this filename exists in our
// Start-menu folder. Filename-keyed (resolved from settings by app id) so a
// server-side display-name change can't orphan a shortcut.
func shortcutExists(filename string) bool {
	d := shortcutsDir()
	if d == "" || filename == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(d, filename))
	return err == nil
}

// removeShortcutFile deletes a .lnk by filename and prunes the folder if empty.
func removeShortcutFile(filename string) error {
	d := shortcutsDir()
	if d == "" || filename == "" {
		return errors.New("no Start menu folder")
	}
	if err := os.Remove(filepath.Join(d, filename)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_ = os.Remove(d) // prune when empty (fails silently otherwise)
	return nil
}

// physicalIconPath translates a path under MSIX AppData virtualization into
// its real on-disk location. Inside the package, writes to %APPDATA%\nimbo are
// redirected to ...\Packages\<PFN>\LocalCache\Roaming\nimbo — the app sees the
// virtual path, but the .lnk icon is loaded by EXPLORER, outside the package,
// where the virtual path resolves to nothing and the icon silently falls back
// to the target exe's. Unpackaged builds pass through unchanged.
func physicalIconPath(p string) string {
	pfn := packageFamilyName()
	if pfn == "" || p == "" {
		return p
	}
	roaming := os.Getenv("APPDATA")
	local := os.Getenv("LOCALAPPDATA")
	if roaming == "" || local == "" {
		return p
	}
	if !strings.HasPrefix(strings.ToLower(p), strings.ToLower(roaming)+`\`) {
		return p
	}
	rel := p[len(roaming)+1:]
	return filepath.Join(local, "Packages", pfn, "LocalCache", "Roaming", rel)
}

// launcherAlias is the AppExecutionAlias filename for this brand. "-app"
// suffixed so it can't collide with the CLI binary (nimbo.exe). Must match
// the manifest's ExecutionAlias (packaging/msix/AppxManifest.xml) and the
// white-label patch in build-partner.ps1.
func launcherAlias() string {
	return strings.ToLower(sanitizeFileName(brand.Current.AppID)) + "-app.exe"
}

// shortcutTarget resolves what the .lnk should launch: packaged installs use
// the stable AppExecutionAlias shim; dev (unpackaged) runs point at the exe.
func shortcutTarget() (string, error) {
	if packageFamilyName() != "" {
		local := os.Getenv("LOCALAPPDATA")
		if local == "" {
			return "", errors.New("LOCALAPPDATA not set")
		}
		p := filepath.Join(local, "Microsoft", "WindowsApps", launcherAlias())
		if _, err := os.Stat(p); err != nil {
			return "", errors.New("this build doesn't register the launcher alias yet — update " + brand.Current.Name + " and try again")
		}
		return p, nil
	}
	return os.Executable()
}

// createAppShortcut writes the Start-menu .lnk: target --app <id>, the app's
// icon, and the window-matching AppUserModelID property.
func createAppShortcut(id, name, aumid, icoPath string) error {
	target, err := shortcutTarget()
	if err != nil {
		return err
	}
	dir := shortcutsDir()
	if dir == "" {
		return errors.New("no Start menu folder")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	// COM wants a consistent apartment; lock the thread for the whole dance.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	const coinitApartment = 0x2
	r, _, _ := procCoInitializeEx.Call(0, coinitApartment)
	// S_OK (0) and S_FALSE (1, already initialised) both mean "usable".
	if r != 0 && r != 1 {
		return windows.Errno(r)
	}
	defer procCoUninitialize.Call()

	var link *iShellLinkW
	const clsctxInproc = 0x1
	r, _, _ = procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(&clsidShellLink)), 0, clsctxInproc,
		uintptr(unsafe.Pointer(&iidIShellLinkW)), uintptr(unsafe.Pointer(&link)))
	if r != 0 || link == nil {
		return windows.Errno(r)
	}
	defer comRelease(unsafe.Pointer(link), link.lpVtbl.Release)

	tp, err := utf16Arg(target)
	if err != nil {
		return err
	}
	if err := hrCall(link.lpVtbl.SetPath, uintptr(unsafe.Pointer(link)), tp); err != nil {
		return err
	}
	argp, err := utf16Arg("--app " + id)
	if err != nil {
		return err
	}
	if err := hrCall(link.lpVtbl.SetArguments, uintptr(unsafe.Pointer(link)), argp); err != nil {
		return err
	}
	desc, err := utf16Arg(name + " — opens in its own window via " + brand.Current.Name)
	if err != nil {
		return err
	}
	_ = hrCall(link.lpVtbl.SetDescription, uintptr(unsafe.Pointer(link)), desc) // cosmetic
	if icoPath != "" {
		// Explorer resolves this path outside the package — it must be physical.
		if ip, err := utf16Arg(physicalIconPath(icoPath)); err == nil {
			_ = hrCall(link.lpVtbl.SetIconLocation, uintptr(unsafe.Pointer(link)), ip, 0) // cosmetic
		}
	}

	// The AUMID property — what makes pinning + window grouping line up.
	var ps *iPropertyStore
	if err := hrCall(link.lpVtbl.QueryInterface, uintptr(unsafe.Pointer(link)),
		uintptr(unsafe.Pointer(&iidIPropertyStore)), uintptr(unsafe.Pointer(&ps))); err != nil {
		return err
	}
	defer ps.Release()
	wa, err := windows.UTF16PtrFromString(aumid)
	if err != nil {
		return err
	}
	pv := propVariant{vt: vtLPWSTR, pwszVal: wa}
	if err := ps.SetValue(&pkeyAppUserModelID, &pv); err != nil {
		return err
	}
	if err := ps.Commit(); err != nil {
		return err
	}

	var pf *iPersistFile
	if err := hrCall(link.lpVtbl.QueryInterface, uintptr(unsafe.Pointer(link)),
		uintptr(unsafe.Pointer(&iidIPersistFile)), uintptr(unsafe.Pointer(&pf))); err != nil {
		return err
	}
	defer comRelease(unsafe.Pointer(pf), pf.lpVtbl.Release)
	// Filename derivation must match what toggleAppShortcut records in settings.
	lp, err := utf16Arg(filepath.Join(dir, sanitizeFileName(name)+".lnk"))
	if err != nil {
		return err
	}
	return hrCall(pf.lpVtbl.Save, uintptr(unsafe.Pointer(pf)), lp, 1)
}
