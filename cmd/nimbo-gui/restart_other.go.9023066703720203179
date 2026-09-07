//go:build !windows

package main

// relaunchSelf is a no-op off Windows (the GUI targets Windows).
func relaunchSelf() {}

// canApplyUpdate / applyUpdate: in-app MSIX self-update is Windows-only.
func canApplyUpdate() bool      { return false }
func applyUpdate(string) error { return nil }
