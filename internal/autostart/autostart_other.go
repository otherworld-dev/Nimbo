//go:build !windows && !linux

package autostart

import "errors"

// errUnsupported is returned by Enable on platforms without autostart support.
var errUnsupported = errors.New("autostart is not supported on this platform yet")

// Supported reports whether autostart can be configured on this platform.
func Supported() bool { return false }

// Enabled always reports false off Windows (not yet implemented).
func Enabled() (bool, error) { return false, nil }

// Enable is unsupported off Windows.
func Enable(string) error { return errUnsupported }

// Disable is a no-op off Windows.
func Disable() error { return nil }
