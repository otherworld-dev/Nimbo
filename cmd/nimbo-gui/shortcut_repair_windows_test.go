//go:build windows

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// End-to-end over the real COM path: write a .lnk the way we ship them, break
// its icon the way a change of app identity breaks it, and check the sweep puts
// it right — for a Start-menu entry AND for a taskbar pin, which is the one
// nothing else in the app ever rewrites.
//
// Everything is sandboxed by APPDATA: shortcutsDir, pinnedTaskbarDir and
// config.Resolve all derive from it, so this never touches the real Start menu.
func TestRepairAppShortcutIcons(t *testing.T) {
	t.Setenv("APPDATA", t.TempDir())

	a := &App{}
	const id = "deck"
	want := a.appIconPath(id)
	if want == "" {
		t.Fatal("no icon path")
	}
	// The icon exists at the new location; the shortcuts don't know that yet.
	if err := ensureAppIconsDir(filepath.Dir(want)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(want, []byte("icon"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A shortcut pointing into the old, now-deleted package data root.
	const dead = `C:\Users\nobody\AppData\Local\Packages\Nimbo_gone\LocalCache\Roaming\nimbo\appicons\deck.3.ico`
	if err := createAppShortcut(id, "Deck", aumidFor(id), dead); err != nil {
		t.Fatal(err)
	}
	start := filepath.Join(shortcutsDir(), "Deck.lnk")

	// Windows' own private copy of it, as a taskbar pin.
	pin := filepath.Join(pinnedTaskbarDir(), "Deck.lnk")
	if err := os.MkdirAll(filepath.Dir(pin), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(start)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pin, b, 0o644); err != nil {
		t.Fatal(err)
	}

	// Someone else's pin must come through the sweep untouched.
	foreign := filepath.Join(pinnedTaskbarDir(), "Firefox.lnk")
	if err := os.WriteFile(foreign, b, 0o644); err != nil {
		t.Fatal(err)
	}
	retarget(t, foreign, `C:\Program Files\Mozilla Firefox\firefox.exe`, "", dead)

	if got := iconOf(t, start); got != dead {
		t.Fatalf("precondition: Start-menu icon = %q; want %q", got, dead)
	}

	a.repairAppShortcutIcons()

	if got := iconOf(t, start); got != want {
		t.Errorf("Start-menu icon = %q; want %q", got, want)
	}
	if got := iconOf(t, pin); got != want {
		t.Errorf("taskbar pin icon = %q; want %q", got, want)
	}
	if got := iconOf(t, foreign); got != dead {
		t.Errorf("someone else's shortcut was rewritten to %q; want it left at %q", got, dead)
	}

	// Idempotent — the sweep runs on every startup and every app-window open.
	a.repairAppShortcutIcons()
	if got := iconOf(t, pin); got != want {
		t.Errorf("second sweep changed the pin icon to %q; want %q", got, want)
	}
}

// A shortcut already carrying the right icon must not be rewritten at all.
func TestRepairShortcutIconSkipsCorrectAndForeign(t *testing.T) {
	t.Setenv("APPDATA", t.TempDir())

	a := &App{}
	const id = "notes"
	want := a.appIconPath(id)
	if err := ensureAppIconsDir(filepath.Dir(want)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(want, []byte("icon"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := createAppShortcut(id, "Notes", aumidFor(id), want); err != nil {
		t.Fatal(err)
	}
	lnk := filepath.Join(shortcutsDir(), "Notes.lnk")

	selfExe, _ := os.Executable()
	var changed bool
	withCOM(t, func() { changed = a.repairShortcutIcon(lnk, launcherAlias(), selfExe) })
	if changed {
		t.Error("a shortcut with the correct icon was rewritten; want it left alone")
	}

	// And one whose icon is dead but which we don't own.
	foreign := filepath.Join(shortcutsDir(), "Firefox.lnk")
	b, err := os.ReadFile(lnk)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreign, b, 0o644); err != nil {
		t.Fatal(err)
	}
	retarget(t, foreign, `C:\Program Files\Mozilla Firefox\firefox.exe`, "", `C:\gone\x.ico`)
	withCOM(t, func() { changed = a.repairShortcutIcon(foreign, launcherAlias(), selfExe) })
	if changed {
		t.Error("someone else's shortcut was rewritten")
	}
}

// If nothing can supply an icon at the new path, the sweep must leave the
// shortcut alone rather than swap one dead path for another.
func TestRepairShortcutIconWithoutAnIcon(t *testing.T) {
	t.Setenv("APPDATA", t.TempDir())

	a := &App{} // no engine: ensureAppIcon can't fetch
	const id = "mail"
	const dead = `C:\gone\mail.3.ico`
	if err := createAppShortcut(id, "Mail", aumidFor(id), dead); err != nil {
		t.Fatal(err)
	}
	lnk := filepath.Join(shortcutsDir(), "Mail.lnk")

	selfExe, _ := os.Executable()
	var changed bool
	withCOM(t, func() { changed = a.repairShortcutIcon(lnk, launcherAlias(), selfExe) })
	if changed {
		t.Error("rewrote the icon path with no icon to point at")
	}
	if got := iconOf(t, lnk); got != dead {
		t.Errorf("icon = %q; want it left at %q", got, dead)
	}
}

// withCOM runs fn on a thread with an initialised apartment — loadShellLink and
// friends are raw COM and need one.
func withCOM(t *testing.T, fn func()) {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	const coinitApartment = 0x2
	r, _, _ := procCoInitializeEx.Call(0, coinitApartment)
	if r != 0 && r != 1 {
		t.Fatalf("CoInitializeEx: %v", windows.Errno(r))
	}
	defer procCoUninitialize.Call()
	fn()
}

// iconOf reads a .lnk's stored icon path.
func iconOf(t *testing.T, path string) string {
	t.Helper()
	var out string
	withCOM(t, func() {
		l, err := loadShellLink(path)
		if err != nil {
			t.Fatalf("load %s: %v", path, err)
		}
		defer l.release()
		out = l.iconLocation()
	})
	return out
}

// retarget rewrites a .lnk's target and arguments, to stand in for a shortcut
// this app didn't create.
func retarget(t *testing.T, path, target, args, ico string) {
	t.Helper()
	withCOM(t, func() {
		l, err := loadShellLink(path)
		if err != nil {
			t.Fatal(err)
		}
		defer l.release()
		self := uintptr(unsafe.Pointer(l.link))
		tp, err := utf16Arg(target)
		if err != nil {
			t.Fatal(err)
		}
		if err := hrCall(l.link.lpVtbl.SetPath, self, tp); err != nil {
			t.Fatalf("SetPath: %v", err)
		}
		ap, err := utf16Arg(args)
		if err != nil {
			t.Fatal(err)
		}
		if err := hrCall(l.link.lpVtbl.SetArguments, self, ap); err != nil {
			t.Fatalf("SetArguments: %v", err)
		}
		if err := l.setIconAndSave(path, ico); err != nil {
			t.Fatalf("save: %v", err)
		}
	})
}
