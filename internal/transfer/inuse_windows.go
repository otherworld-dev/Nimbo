//go:build windows

package transfer

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// writerPresent reports, as a sharing violation, whether some program has path
// open with write access right now. The probe asks for read access while
// refusing to share write, which Windows grants only if no handle already has
// write access, and closes again at once: it holds nothing, so it can't get in
// a writer's way.
func writerPresent(path string) error {
	p, err := windows.UTF16PtrFromString(extendedPath(path))
	if err != nil {
		return nil
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return err
		}
		return nil // anything else is for the upload itself to report
	}
	windows.CloseHandle(h)
	return nil
}

// extendedPath gives CreateFile the \?\ form of a long absolute path, which
// the os package does for itself but a raw CreateFile call does not.
func extendedPath(path string) string {
	if len(path) < 248 || strings.HasPrefix(path, `\?\`) || strings.HasPrefix(path, `\.\`) || !filepath.IsAbs(path) {
		return path
	}
	path = filepath.Clean(path)
	if strings.HasPrefix(path, `\`) {
		return `\?\UNC\` + path[2:]
	}
	return `\?\` + path
}

// openShared opens path to read the way os.Open does, but sharing delete as
// well, so an editor's atomic save (a temp file renamed over this one) isn't
// refused for as long as Nimbo reads the file: minutes, on a large upload.
// The handle goes on reading the version it opened; the replacement is a new
// file, which the next pass uploads.
func openShared(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(extendedPath(path))
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}
