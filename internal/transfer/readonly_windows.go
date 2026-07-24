//go:build windows

package transfer

import "os"

// setReadOnly mirrors a server read-only flag onto the local path. On Windows
// this toggles FILE_ATTRIBUTE_READONLY (what os.Chmod does for the missing
// owner-write bit). The read-only attribute on a DIRECTORY is advisory — it
// does NOT prevent the sync process from creating children inside it (unlike a
// deny ACL), so it's safe to mark read-only folders here.
func setReadOnly(path string, ro bool) error {
	mode := os.FileMode(0o644)
	if ro {
		mode = 0o444
	}
	return os.Chmod(path, mode)
}
