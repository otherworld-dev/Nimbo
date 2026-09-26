//go:build windows

package main

import (
	"fmt"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// volumeID identifies the disk a path is on (its volume serial number), or ""
// when it can't be read. The path itself need not exist, only its drive: a
// different disk given the same drive letter reads differently.
func volumeID(path string) string {
	vol := filepath.VolumeName(filepath.Clean(path))
	if vol == "" {
		return ""
	}
	root, err := windows.UTF16PtrFromString(vol + `\`)
	if err != nil {
		return ""
	}
	var serial uint32
	if err := windows.GetVolumeInformation(root, nil, 0, &serial, nil, nil, nil, 0); err != nil {
		return ""
	}
	return fmt.Sprintf("%08x", serial)
}
