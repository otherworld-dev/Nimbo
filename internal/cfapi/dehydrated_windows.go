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
