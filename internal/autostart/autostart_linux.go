//go:build linux

// Package autostart on Linux manages a freedesktop autostart entry: a
// nimbo.desktop file under $XDG_CONFIG_HOME/autostart (~/.config/autostart),
// which compliant desktop environments (GNOME, KDE, XFCE, …) launch at login.
package autostart

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const desktopName = "nimbo.desktop"

// autostartDir is $XDG_CONFIG_HOME/autostart (falling back to ~/.config).
func autostartDir() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "autostart")
}

func desktopPath() string { return filepath.Join(autostartDir(), desktopName) }

// Supported reports whether autostart can be configured on this platform.
func Supported() bool { return true }

// Enabled reports whether the autostart entry exists and isn't explicitly
// disabled (some desktops flip X-GNOME-Autostart-enabled rather than deleting).
func Enabled() (bool, error) {
	b, err := os.ReadFile(desktopPath())
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if strings.Contains(string(b), "X-GNOME-Autostart-enabled=false") {
		return false, nil
	}
	return true, nil
}

// Enable writes the autostart .desktop entry launching exePath at login.
func Enable(exePath string) error {
	if err := os.MkdirAll(autostartDir(), 0o755); err != nil {
		return err
	}
	exec := exePath
	if strings.ContainsAny(exePath, " \t") {
		exec = `"` + exePath + `"` // a path with spaces must be quoted in Exec=
	}
	content := "[Desktop Entry]\n" +
		"Type=Application\n" +
		"Name=Nimbo\n" +
		"Comment=Nextcloud sync client\n" +
		fmt.Sprintf("Exec=%s\n", exec) +
		"Terminal=false\n" +
		"X-GNOME-Autostart-enabled=true\n"
	return os.WriteFile(desktopPath(), []byte(content), 0o644)
}

// Disable removes the autostart entry (no-op if absent).
func Disable() error {
	if err := os.Remove(desktopPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
