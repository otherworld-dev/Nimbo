//go:build linux

package transfer

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplace renames from to to, failing with an error matching
// os.ErrExist when something is already at to. renameat2(RENAME_NOREPLACE)
// does it in one call; a file system without it falls back to look-then-rename.
func renameNoReplace(from, to string) error {
	err := unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EEXIST):
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: os.ErrExist}
	case errors.Is(err, unix.ENOSYS), errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOTSUP):
		return renameNoReplaceFallback(from, to)
	}
	return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
}
