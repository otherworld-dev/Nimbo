//go:build !windows

package main

import (
	"errors"
	"unsafe"
)

// Per-window taskbar identity and Start-menu shortcuts are Windows-only; on
// other platforms app windows still open, they just share the app's identity.

func setAppWindowIdentity(hwnd unsafe.Pointer, id, aumid, icoPath string) {}

func refreshWindowIcon(hwnd unsafe.Pointer, id, icoPath string) {}

func shortcutExists(filename string) bool { return false }

func removeShortcutFile(filename string) error { return errors.New("not supported on this platform") }

func createAppShortcut(id, name, aumid, icoPath string) error {
	return errors.New("not supported on this platform")
}
