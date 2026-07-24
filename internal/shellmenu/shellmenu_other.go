//go:build !windows

// Package shellmenu manages OS file-manager "Share" integration. Only Windows
// Explorer is supported for now; other platforms are no-ops.
package shellmenu

// Supported reports whether the file-manager integration can be configured here.
func Supported() bool { return false }

// Enabled reports whether the integration is currently registered.
func Enabled() bool { return false }

// Register is a no-op on non-Windows platforms.
func Register(string) error { return nil }

// Unregister is a no-op on non-Windows platforms.
func Unregister() error { return nil }
