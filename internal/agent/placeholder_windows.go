package agent

import (
	"os"
	"syscall"
)

// isDehydratedPlaceholder reports whether fi describes a file whose contents
// are not on local disk. See placeholderAttrs for which attributes qualify.
func isDehydratedPlaceholder(fi os.FileInfo) bool {
	if fi == nil {
		return false
	}
	d, ok := fi.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return false
	}
	return placeholderAttrs(d.FileAttributes)
}
