//go:build !windows

// Package shellns is a no-op outside Windows (the Explorer navigation pane is a
// Windows-only concept).
package shellns

// Supported reports whether the sidebar entry can be configured here.
func Supported() bool { return false }

// Enabled reports whether the sidebar node is registered.
func Enabled() bool { return false }

// Register is a no-op on non-Windows platforms.
func Register(string, string, string) error { return nil }

// Unregister is a no-op on non-Windows platforms.
func Unregister() error { return nil }
