//go:build windows

// Package autostart manages whether Nimbo launches at login. On Windows
// this is an HKCU "Run" registry value.
package autostart

import "golang.org/x/sys/windows/registry"

const (
	runKey    = `Software\Microsoft\Windows\CurrentVersion\Run`
	valueName = "Nimbo"
)

// Supported reports whether autostart can be configured on this platform.
func Supported() bool { return true }

// Enabled reports whether Nimbo is set to start at login.
func Enabled() (bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false, err
	}
	defer k.Close()
	_, _, err = k.GetStringValue(valueName)
	if err == registry.ErrNotExist {
		return false, nil
	}
	return err == nil, err
}

// Enable sets Nimbo to launch exePath at login.
func Enable(exePath string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(valueName, `"`+exePath+`"`)
}

// Disable removes the login entry (no-op if absent).
func Disable() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.DeleteValue(valueName); err != nil && err != registry.ErrNotExist {
		return err
	}
	return nil
}
