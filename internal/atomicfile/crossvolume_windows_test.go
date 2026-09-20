package atomicfile

import "golang.org/x/sys/windows"

// crossVolumeErr is what Windows really returns; verified against a genuine
// C:->E: rename, which renders as "The system cannot move the file to a
// different disk drive" — the wording in issue #8.
func crossVolumeErr() error { return windows.ERROR_NOT_SAME_DEVICE }
