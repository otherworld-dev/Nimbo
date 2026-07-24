//go:build !windows

package transfer

import "os"

// setReadOnly mirrors a server read-only flag onto the local path. On Unix a
// read-only DIRECTORY (no write bit) would block the sync process from creating
// children inside it, so only FILES are marked read-only here; directories keep
// their mode (the read-only intent is Windows-centric).
func setReadOnly(path string, ro bool) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return nil
	}
	mode := os.FileMode(0o644)
	if ro {
		mode = 0o444
	}
	return os.Chmod(path, mode)
}
