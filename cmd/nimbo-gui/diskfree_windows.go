//go:build windows

package main

import (
	"path/filepath"

	"golang.org/x/sys/windows"
)

// diskFree returns the number of free bytes available on the volume containing
// path (0 if it can't be determined).
func diskFree(path string) uint64 {
	if path == "" {
		return 0
	}
	// GetDiskFreeSpaceEx accepts a directory; use the volume root if needed.
	p, err := windows.UTF16PtrFromString(filepath.VolumeName(path) + `\`)
	if err != nil {
		return 0
	}
	var freeAvail, total, totalFree uint64
	err = windows.GetDiskFreeSpaceEx(p, &freeAvail, &total, &totalFree)
	if err != nil {
		return 0
	}
	return freeAvail
}
