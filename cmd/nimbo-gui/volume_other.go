//go:build !windows

package main

// volumeID is not tracked outside Windows (virtual files are Windows-only).
func volumeID(string) string { return "" }
