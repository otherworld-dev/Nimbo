//go:build !windows

package main

// setDPIAwareness is a no-op off Windows: DPI awareness is a Win32 process
// attribute. On Linux the toolkit and compositor handle scaling.
func setDPIAwareness() error { return nil }
