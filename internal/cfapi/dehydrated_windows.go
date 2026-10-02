package cfapi

import (
	"os"
	"syscall"
)

// IsDehydrated reports whether fi describes a file whose contents
// are not on local disk. See PlaceholderAttrs for which attributes qualify.
func IsDehydrated(fi os.FileInfo) bool {
	if fi == nil {
		return false
	}
	d, ok := fi.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return false
	}
	return PlaceholderAttrs(d.FileAttributes)
}

// ClearStrayOffline takes FILE_ATTRIBUTE_OFFLINE off path when it is a stray
// (see strayOffline), reporting whether it did. Left on, the file reads as
// dehydrated: an edit is taken for an online-only stub and never uploaded, and
// once the upload converts it to a placeholder it reads as one for good.
func ClearStrayOffline(path string) (bool, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	attrs, err := syscall.GetFileAttributes(p)
	if err != nil {
		return false, err
	}
	if !strayOffline(attrs) {
		return false, nil
	}
	// SetFileAttributes takes only the settable bits; the rest (sparse,
	// compressed, encrypted ...) are the filesystem's and are passed as 0.
	const settable = 0x1 | 0x2 | 0x4 | 0x20 | 0x100 | 0x2000 | 0x80000 | 0x100000 // READONLY HIDDEN SYSTEM ARCHIVE TEMPORARY NOT_CONTENT_INDEXED PINNED UNPINNED
	next := attrs & settable
	if next == 0 {
		next = syscall.FILE_ATTRIBUTE_NORMAL
	}
	if err := syscall.SetFileAttributes(p, next); err != nil {
		return false, err
	}
	return true, nil
}
