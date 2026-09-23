//go:build !windows

// Package cfapi is a no-op outside Windows (on-demand files are a Windows
// Cloud Files API feature).
package cfapi

import (
	"context"
	"errors"
	"io"
	"os"
	"time"
)

// IsDehydrated always reports false outside Windows: the placeholder attributes
// it looks for are a Windows filesystem concept, and no supported non-Windows
// provider leaves hollow files behind for a sync to trip over.
func IsDehydrated(os.FileInfo) bool { return false }

// PlaceholderInfo mirrors the Windows type so cross-platform code referencing it
// (e.g. the vfs stub) compiles.
type PlaceholderInfo struct {
	Name       string
	Size       int64
	IsDir      bool
	ModTime    time.Time
	Identity   []byte
	ETag       string
	UploadTime int64
	FileID     string
	MountRoot  bool
	Encrypted  bool
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

// HydrateStreamFunc mirrors the Windows provider's streaming hydration type.
type HydrateStreamFunc func(ctx context.Context, identity []byte, offset, length int64) (io.ReadCloser, error)

// SetHydrateStream is a no-op on non-Windows platforms (cross-platform code
// calls it unconditionally after a Mount that cannot succeed here anyway).
func SetHydrateStream(int64, HydrateStreamFunc) {}

// RenameFunc mirrors the Windows provider's rename-completion callback type.
type RenameFunc func(oldPath, newPath string)

// SetRenameHandler is a no-op on non-Windows platforms (cross-platform code,
// e.g. the vfs watcher, calls it unconditionally).
func SetRenameHandler(int64, RenameFunc) {}

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

// UpdateIdentityKeepState is unavailable off Windows.
func UpdateIdentityKeepState(string, []byte) error {
	return errors.New("on-demand files are Windows-only")
}

// RevertPlaceholder is unavailable off Windows.
func RevertPlaceholder(string) error { return errors.New("on-demand files are Windows-only") }

// UnregisterLegacyShellSyncRoot is a no-op on non-Windows platforms.
func UnregisterLegacyShellSyncRoot() {}

// RegisterStatusRoot is unavailable off Windows (Supported() gates all callers).
func RegisterStatusRoot(string) error { return errors.New("cloud sync roots are Windows-only") }

// ShellSyncRootRegistered is always false off Windows: there is no Explorer.
func ShellSyncRootRegistered(string) bool { return false }

// ShellSyncRootNamespaceCLSID is always empty off Windows.
func ShellSyncRootNamespaceCLSID(string) string { return "" }

// IsPlaceholder is always false off Windows.
func IsPlaceholder(string) (bool, error) { return false, nil }

// Disconnect is a no-op off Windows.
func Disconnect(string, int64) {}

// ShellNotifyUpdated is Windows-only (Explorer glyph refresh); no-op elsewhere.
func ShellNotifyUpdated(string) {}

// ShellNotifyCreated is Windows-only (Explorer change notification); no-op elsewhere.
func ShellNotifyCreated(string, bool) {}

// ExcludeFromSync is Windows-only (cloud-filter pin state); no-op elsewhere.
func ExcludeFromSync(string) error { return nil }

// ExposePlaceholders is Windows-only; no-op elsewhere.
func ExposePlaceholders() {}

// PlaceholderModified is unavailable off Windows (no cloud placeholders).
func PlaceholderModified(string) (bool, error) {
	return false, errors.New("on-demand files are Windows-only")
}

// SetInSync is unavailable off Windows (no cloud placeholders to mark).
func SetInSync(string) error { return errors.New("on-demand files are Windows-only") }
