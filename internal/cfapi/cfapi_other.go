//go:build !windows

// Package cfapi is a no-op outside Windows (on-demand files are a Windows
// Cloud Files API feature).
package cfapi

import (
	"errors"
	"time"
)

// PlaceholderInfo mirrors the Windows type so cross-platform code referencing it
// (e.g. the vfs stub) compiles.
type PlaceholderInfo struct {
	Name     string
	Size     int64
	IsDir    bool
	ModTime  time.Time
	Identity []byte
	ETag     string
	FileID   string
}

// Debug is a diagnostic hook (used on Windows); unused here.
var Debug func(format string, args ...any)

// Supported reports whether on-demand files can be configured here.
func Supported() bool { return false }

// RegisterSyncRoot is a no-op on non-Windows platforms.
func RegisterSyncRoot(string) error { return nil }

// UnregisterSyncRoot is a no-op on non-Windows platforms.
func UnregisterSyncRoot(string) error { return nil }

// Purge is a no-op on non-Windows platforms.
func Purge(string) error { return nil }

// HydrateFunc / ListFunc mirror the Windows provider callback types.
type HydrateFunc func(identity []byte, offset, length int64) ([]byte, error)
type ListFunc func(rel string) []PlaceholderInfo

// Mount is unavailable off Windows (Supported() gates all callers).
func Mount(string, string, string, HydrateFunc, ListFunc) (int64, error) {
	return 0, errors.New("on-demand files are Windows-only")
}

// Unmount is a no-op on non-Windows platforms.
func Unmount(string, int64) {}

// SetPinState is unavailable off Windows (Supported() gates all callers).
func SetPinState(string, bool, bool) error { return errors.New("on-demand files are Windows-only") }

// PinStateOf reports no preference on non-Windows platforms.
func PinStateOf(string) string { return "" }

// Dehydrate is unavailable off Windows.
func Dehydrate(string) error { return errors.New("on-demand files are Windows-only") }

// UnregisterLegacyShellSyncRoot is a no-op on non-Windows platforms.
func UnregisterLegacyShellSyncRoot() {}
