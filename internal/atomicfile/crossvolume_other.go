//go:build !windows

package atomicfile

import (
	"errors"
	"syscall"
)

// isCrossVolume reports whether err is the kernel refusing to rename across
// filesystems, which a bind mount or a separate mount for ~/.config can make
// happen with two paths that look like siblings.
func isCrossVolume(err error) bool {
	return errors.Is(err, syscall.EXDEV)
}
