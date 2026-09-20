//go:build windows

// Package autostart manages whether the app launches at sign-in.
//
// On Windows there are two mechanisms, and which one applies depends on how
// the process was started:
//
//   - PACKAGED (MSIX) builds are started by the startup task declared in
//     packaging/msix/AppxManifest.xml. Windows runs that task by itself; the app
//     switches it on and off through the WinRT StartupTask API
//     (startuptask_windows.go).
//   - UNPACKAGED builds (a loose dev exe) have no package identity and no
//     startup task, so they use an HKCU "Run" registry value.
//
// Packaged builds used to write the Run value as well, which is why they kept
// starting at sign-in with the box unticked. Inside the package container every
// HKCU write is virtualised into the package's private hive, so Windows never
// saw the value; reading it back read the same private hive, so the checkbox
// showed whatever was last clicked. Meanwhile the manifest's task
// (Enabled="true") ran at every sign-in and nothing ever turned it off. It is
// the same container trap internal/shellns documents.
package autostart

import (
	"errors"
	"fmt"

	"github.com/otherworld/nimbo/internal/shellns"
	"golang.org/x/sys/windows/registry"
)

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

// runValueName is the Run value unpackaged builds write. A var only so the
// live test can use a throwaway name instead of touching a real entry.
var runValueName = "Nimbo"

// startupState is Windows.ApplicationModel.StartupTaskState, an int32 on the
// ABI. Values checked against the Windows SDK 10.0.26100.0 headers
// (winrt\windows.applicationmodel.h, `enum StartupTaskState : int`, and the
// matching .idl), not recalled.
type startupState int32

const (
	stateDisabled         startupState = 0
	stateDisabledByUser   startupState = 1
	stateEnabled          startupState = 2
	stateDisabledByPolicy startupState = 3 // StartupTaskContract 2.0
	stateEnabledByPolicy  startupState = 4 // StartupTaskContract 3.0
)

func (s startupState) String() string {
	switch s {
	case stateDisabled:
		return "Disabled"
	case stateDisabledByUser:
		return "DisabledByUser"
	case stateEnabled:
		return "Enabled"
	case stateDisabledByPolicy:
		return "DisabledByPolicy"
	case stateEnabledByPolicy:
		return "EnabledByPolicy"
	}
	return fmt.Sprintf("StartupTaskState(%d)", int32(s))
}

// on reports whether Windows will start the app at sign-in in this state.
func (s startupState) on() bool { return s == stateEnabled || s == stateEnabledByPolicy }

// User-facing refusals. SetAutostart (cmd/nimbo-gui/service.go) hands
// err.Error() unchanged to the Settings page, which shows it in an alert and
// flips the checkbox back, so these are sentences for a person. They name no
// product because white-label builds run this code too.
var (
	errDisabledByUser  = errors.New("Windows has turned this off. Turn it back on in Windows Settings > Apps > Startup.")
	errManagedByPolicy = errors.New("Your organisation manages this setting, so it can't be changed here.")
	errTimedOut        = errors.New("Windows took too long to answer. Try again in a moment.")
)

// enableResult turns the state RequestEnableAsync settled on into Enable's
// result. A full-trust app gets no consent dialog; Windows just answers with
// the state, and it refuses outright when the user or a policy turned the task
// off.
func enableResult(s startupState) error {
	switch s {
	case stateEnabled, stateEnabledByPolicy:
		return nil
	case stateDisabledByUser:
		return errDisabledByUser
	case stateDisabledByPolicy:
		return errManagedByPolicy
	}
	return fmt.Errorf("Windows left this turned off (startup task state %s). Check Windows Settings > Apps > Startup.", s)
}

// disableResult turns the state after Disable into Disable's result. Only a
// policy can keep the task on once the app has turned it off; saying so lets
// the Settings page put the tick back instead of showing a lie.
func disableResult(s startupState) error {
	if s == stateEnabledByPolicy {
		return errManagedByPolicy
	}
	return nil
}

// Seams. The public functions below only decide; these do the work. Tests swap
// them to drive both paths without package identity or real WinRT calls.
var (
	packaged          = shellns.Packaged
	taskState         = startupTaskState
	taskRequestEnable = startupTaskRequestEnable
	taskDisable       = startupTaskDisable
	runEnabled        = runKeyEnabled
	runEnable         = runKeyEnable
	runDisable        = runKeyDisable
)

// Supported reports whether autostart can be configured on this platform.
func Supported() bool { return true }

// Enabled reports whether Windows will start the app at sign-in. On a packaged
// build that is the startup task's real state, not anything the app wrote.
func Enabled() (bool, error) {
	if !packaged() {
		return runEnabled()
	}
	s, err := taskState()
	if err != nil {
		return false, err
	}
	return s.on(), nil
}

// Enable makes the app start at sign-in. exePath is only used by unpackaged
// builds; the packaged startup task names its own executable in the manifest.
func Enable(exePath string) error {
	if !packaged() {
		return runEnable(exePath)
	}
	// Older packaged versions left a Run value in the package's private hive.
	// Windows never read it, but remove it so nothing can mistake it for the
	// real setting. Best effort: it is usually not there.
	_ = runDisable()
	s, err := taskRequestEnable()
	if err != nil {
		return err
	}
	return enableResult(s)
}

// Disable stops the app starting at sign-in (a no-op if it already doesn't).
func Disable() error {
	if !packaged() {
		return runDisable()
	}
	_ = runDisable() // stale virtualised value, as in Enable
	s, err := taskDisable()
	if err != nil {
		return err
	}
	return disableResult(s)
}

// --- the unpackaged path: an HKCU Run value ---

func runKeyEnabled() (bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false, err
	}
	defer k.Close()
	_, _, err = k.GetStringValue(runValueName)
	if err == registry.ErrNotExist {
		return false, nil
	}
	return err == nil, err
}

func runKeyEnable(exePath string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(runValueName, `"`+exePath+`"`)
}

func runKeyDisable() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.DeleteValue(runValueName); err != nil && err != registry.ErrNotExist {
		return err
	}
	return nil
}
