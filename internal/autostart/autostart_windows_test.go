//go:build windows

package autostart

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// stub swaps a seam for the duration of one test.
func stub[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// The values Windows uses, restated from the SDK so a typo in the constants
// fails a test rather than a release.
func TestStartupStateValues(t *testing.T) {
	for want, s := range []startupState{stateDisabled, stateDisabledByUser, stateEnabled, stateDisabledByPolicy, stateEnabledByPolicy} {
		if int32(s) != int32(want) {
			t.Errorf("%s = %d, want %d", s, int32(s), want)
		}
	}
}

func TestStartupStateOn(t *testing.T) {
	cases := map[startupState]bool{
		stateDisabled:         false,
		stateDisabledByUser:   false,
		stateEnabled:          true,
		stateDisabledByPolicy: false,
		stateEnabledByPolicy:  true,
		startupState(7):       false, // a state a future Windows might add
	}
	for s, want := range cases {
		if got := s.on(); got != want {
			t.Errorf("%s.on() = %v, want %v", s, got, want)
		}
	}
}

func TestEnableResult(t *testing.T) {
	for _, s := range []startupState{stateEnabled, stateEnabledByPolicy} {
		if err := enableResult(s); err != nil {
			t.Errorf("enableResult(%s) = %v, want nil", s, err)
		}
	}
	if err := enableResult(stateDisabledByUser); !errors.Is(err, errDisabledByUser) {
		t.Errorf("enableResult(DisabledByUser) = %v, want errDisabledByUser", err)
	}
	if err := enableResult(stateDisabledByPolicy); !errors.Is(err, errManagedByPolicy) {
		t.Errorf("enableResult(DisabledByPolicy) = %v, want errManagedByPolicy", err)
	}
	for _, s := range []startupState{stateDisabled, startupState(7)} {
		err := enableResult(s)
		if err == nil {
			t.Errorf("enableResult(%s) = nil, want an error", s)
			continue
		}
		if !strings.Contains(err.Error(), s.String()) {
			t.Errorf("enableResult(%s) = %q, want it to name the state", s, err)
		}
	}
}

func TestDisableResult(t *testing.T) {
	if err := disableResult(stateEnabledByPolicy); !errors.Is(err, errManagedByPolicy) {
		t.Errorf("disableResult(EnabledByPolicy) = %v, want errManagedByPolicy", err)
	}
	for _, s := range []startupState{stateDisabled, stateDisabledByUser, stateEnabled, stateDisabledByPolicy} {
		if err := disableResult(s); err != nil {
			t.Errorf("disableResult(%s) = %v, want nil", s, err)
		}
	}
}

// The Settings page shows these verbatim, and white-label builds run the same
// code, so they must read as instructions and carry no product name.
func TestRefusalMessages(t *testing.T) {
	if got, want := errDisabledByUser.Error(), "Windows has turned this off. Turn it back on in Windows Settings > Apps > Startup."; got != want {
		t.Errorf("errDisabledByUser = %q, want %q", got, want)
	}
	if !strings.Contains(errManagedByPolicy.Error(), "organisation manages this setting") {
		t.Errorf("errManagedByPolicy = %q, want it to say the organisation manages it", errManagedByPolicy)
	}
	for _, err := range []error{errDisabledByUser, errManagedByPolicy, errTimedOut, enableResult(stateDisabled)} {
		if strings.Contains(strings.ToLower(err.Error()), "nimbo") {
			t.Errorf("%q names the product; white-label builds show it too", err)
		}
	}
}

// fakes records which seams ran, so a test can assert one path never touches
// the other's mechanism.
type fakes struct {
	calls []string
	state startupState
	err   error // returned by the task seams
}

func (f *fakes) install(t *testing.T, isPackaged bool) {
	stub(t, &packaged, func() bool { return isPackaged })
	stub(t, &taskState, func() (startupState, error) {
		f.calls = append(f.calls, "taskState")
		return f.state, f.err
	})
	stub(t, &taskRequestEnable, func() (startupState, error) {
		f.calls = append(f.calls, "taskRequestEnable")
		return f.state, f.err
	})
	stub(t, &taskDisable, func() (startupState, error) {
		f.calls = append(f.calls, "taskDisable")
		return f.state, f.err
	})
	stub(t, &runEnabled, func() (bool, error) {
		f.calls = append(f.calls, "runEnabled")
		return true, nil
	})
	stub(t, &runEnable, func(exe string) error {
		f.calls = append(f.calls, "runEnable:"+exe)
		return nil
	})
	stub(t, &runDisable, func() error {
		f.calls = append(f.calls, "runDisable")
		return errors.New("access denied") // must be ignored on the packaged path
	})
}

func (f *fakes) want(t *testing.T, calls ...string) {
	t.Helper()
	if strings.Join(f.calls, ",") != strings.Join(calls, ",") {
		t.Errorf("calls = %v, want %v", f.calls, calls)
	}
	f.calls = nil
}

func TestUnpackagedUsesRunKeyOnly(t *testing.T) {
	f := &fakes{}
	f.install(t, false)

	if ok, err := Enabled(); err != nil || !ok {
		t.Errorf("Enabled() = %v, %v; want the Run key's answer (true, nil)", ok, err)
	}
	f.want(t, "runEnabled")

	if err := Enable(`C:\dev\nimbo-gui.exe`); err != nil {
		t.Errorf("Enable: %v", err)
	}
	f.want(t, `runEnable:C:\dev\nimbo-gui.exe`)

	if err := Disable(); err == nil || err.Error() != "access denied" {
		t.Errorf("Disable = %v; unpackaged must return the Run key's own error", err)
	}
	f.want(t, "runDisable")
}

func TestPackagedEnabledReadsTaskState(t *testing.T) {
	f := &fakes{}
	f.install(t, true)
	for s, want := range map[startupState]bool{
		stateDisabled: false, stateDisabledByUser: false, stateEnabled: true,
		stateDisabledByPolicy: false, stateEnabledByPolicy: true,
	} {
		f.state = s
		if ok, err := Enabled(); err != nil || ok != want {
			t.Errorf("state %s: Enabled() = %v, %v; want %v, nil", s, ok, err, want)
		}
		f.want(t, "taskState")
	}

	f.err = errors.New("GetAsync failed")
	f.state = stateEnabled
	if ok, err := Enabled(); err == nil || ok {
		t.Errorf("WinRT failure: Enabled() = %v, %v; want false and the error", ok, err)
	}
	f.want(t, "taskState")
}

func TestPackagedEnable(t *testing.T) {
	f := &fakes{}
	f.install(t, true)

	f.state = stateEnabled
	if err := Enable(`C:\Program Files\WindowsApps\x\nimbo-gui.exe`); err != nil {
		t.Errorf("Enable (Enabled) = %v, want nil", err)
	}
	// The stale Run value is cleared (its error ignored); nothing is written to it.
	f.want(t, "runDisable", "taskRequestEnable")

	f.state = stateEnabledByPolicy
	if err := Enable("x"); err != nil {
		t.Errorf("Enable (EnabledByPolicy) = %v, want nil", err)
	}
	f.want(t, "runDisable", "taskRequestEnable")

	f.state = stateDisabledByUser
	if err := Enable("x"); err == nil || err.Error() != errDisabledByUser.Error() {
		t.Errorf("Enable (DisabledByUser) = %v, want %q", err, errDisabledByUser)
	}
	f.want(t, "runDisable", "taskRequestEnable")

	f.state = stateDisabledByPolicy
	if err := Enable("x"); !errors.Is(err, errManagedByPolicy) {
		t.Errorf("Enable (DisabledByPolicy) = %v, want errManagedByPolicy", err)
	}
	f.want(t, "runDisable", "taskRequestEnable")

	f.err = errors.New("RequestEnableAsync failed")
	if err := Enable("x"); err == nil || err.Error() != "RequestEnableAsync failed" {
		t.Errorf("Enable (WinRT failure) = %v, want the WinRT error", err)
	}
	f.want(t, "runDisable", "taskRequestEnable")
}

func TestPackagedDisable(t *testing.T) {
	f := &fakes{}
	f.install(t, true)

	f.state = stateDisabled
	if err := Disable(); err != nil {
		t.Errorf("Disable = %v, want nil", err)
	}
	f.want(t, "runDisable", "taskDisable")

	f.state = stateDisabledByUser // already off in Windows Settings: fine
	if err := Disable(); err != nil {
		t.Errorf("Disable (DisabledByUser) = %v, want nil", err)
	}
	f.want(t, "runDisable", "taskDisable")

	f.state = stateEnabledByPolicy
	if err := Disable(); !errors.Is(err, errManagedByPolicy) {
		t.Errorf("Disable (EnabledByPolicy) = %v, want errManagedByPolicy", err)
	}
	f.want(t, "runDisable", "taskDisable")

	f.err = errors.New("Disable failed")
	if err := Disable(); err == nil || err.Error() != "Disable failed" {
		t.Errorf("Disable (WinRT failure) = %v, want the WinRT error", err)
	}
	f.want(t, "runDisable", "taskDisable")
}

// A stuck call must come back as errTimedOut, both while it runs and for a
// later call queued behind it, and the thread must recover once it unsticks.
// This starts the real WinRT thread (RoInitialize only; harmless unpackaged)
// but makes no WinRT call.
func TestWinrtCallTimesOut(t *testing.T) {
	stub(t, &callTimeout, 100*time.Millisecond)

	if v, err := winrtCall(func() (int, error) { return 42, nil }); err != nil || v != 42 {
		t.Fatalf("winrtCall = %v, %v; want 42, nil", v, err)
	}

	release := make(chan struct{})
	start := time.Now()
	if _, err := winrtCall(func() (int, error) { <-release; return 1, nil }); !errors.Is(err, errTimedOut) {
		t.Fatalf("stuck call: err = %v, want errTimedOut", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("stuck call took %v to time out", d)
	}
	if _, err := winrtCall(func() (int, error) { return 2, nil }); !errors.Is(err, errTimedOut) {
		t.Fatalf("call queued behind a stuck one: err = %v, want errTimedOut", err)
	}

	close(release)
	stub(t, &callTimeout, 5*time.Second)
	if v, err := winrtCall(func() (int, error) { return 3, nil }); err != nil || v != 3 {
		t.Fatalf("after the stuck call finished: %v, %v; want 3, nil", v, err)
	}
}
