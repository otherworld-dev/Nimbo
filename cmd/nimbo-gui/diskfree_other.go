//go:build !windows

package main

// diskFree is unavailable off Windows (the GUI targets Windows).
func diskFree(string) uint64 { return 0 }
