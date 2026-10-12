//go:build windows

package transfer

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// renameNoReplace renames from to to, failing with an error matching
// os.ErrExist when something else is already at to. One call to the OS, so
// there is no gap between looking and renaming for a file to appear in.
//
// MoveFileEx without MOVEFILE_REPLACE_EXISTING refuses an existing target. A
// rename that only changes the capitals of the same file is not refused: the
// file system treats it as the rename of that one file.
func renameNoReplace(from, to string) error {
	fp, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	tp, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	if err := windows.MoveFileEx(fp, tp, 0); err != nil {
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS) {
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: os.ErrExist}
		}
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
	return nil
}
