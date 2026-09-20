package atomicfile

import (
	"errors"

	"golang.org/x/sys/windows"
)

// isCrossVolume reports whether err is Windows refusing to move a file between
// volumes ("The system cannot move the file to a different disk drive").
//
// syscall.EXDEV is NOT the same thing here: Go gives that name a placeholder
// value on Windows which no API ever returns, so testing for it silently never
// matches. The real code is ERROR_NOT_SAME_DEVICE (17).
func isCrossVolume(err error) bool {
	return errors.Is(err, windows.ERROR_NOT_SAME_DEVICE)
}
