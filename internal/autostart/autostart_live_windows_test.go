//go:build windows

package autostart

import (
	"os"
	"testing"

	"golang.org/x/sys/windows/registry"
)

// Live tests touch the real registry and WinRT, so they only run on request:
//
//	NIMBO_AUTOSTART_LIVE=1 go test -run Live -v ./internal/autostart/
//
// They never change the installed package's startup task: `go test` runs
// unpackaged, and the WinRT probe below only reads.
func requireLive(t *testing.T) {
	t.Helper()
	if os.Getenv("NIMBO_AUTOSTART_LIVE") != "1" {
		t.Skip("set NIMBO_AUTOSTART_LIVE=1 to run")
	}
	if packaged() {
		t.Skip("running with package identity; these tests are for unpackaged processes only")
	}
}

// The unpackaged path end to end, against the real HKCU Run key, under a
// throwaway value name so a real entry is never touched.
func TestLiveUnpackagedRunKey(t *testing.T) {
	requireLive(t)
	const name = "NimboAutostartLiveTest"
	stub(t, &runValueName, name)
	remove := func() {
		if k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE); err == nil {
			_ = k.DeleteValue(name)
			k.Close()
		}
	}
	remove() // a run that crashed before its cleanup
	t.Cleanup(remove)

	// A path that does not exist, so a crash before cleanup starts nothing at sign-in.
	exe := `C:\nonexistent\nimbo-autostart-live-test.exe`

	if ok, err := Enabled(); err != nil || ok {
		t.Fatalf("before Enable: Enabled() = %v, %v; want false, nil", ok, err)
	}
	if err := Enable(exe); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	v, _, err := k.GetStringValue(name)
	k.Close()
	if err != nil || v != `"`+exe+`"` {
		t.Fatalf("Run value = %q, %v; want the quoted exe path", v, err)
	}
	if ok, err := Enabled(); err != nil || !ok {
		t.Fatalf("after Enable: Enabled() = %v, %v; want true, nil", ok, err)
	}
	if err := Disable(); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if ok, err := Enabled(); err != nil || ok {
		t.Fatalf("after Disable: Enabled() = %v, %v; want false, nil", ok, err)
	}
	if err := Disable(); err != nil {
		t.Fatalf("Disable when already absent: %v", err)
	}
}

// A read-only probe of the WinRT plumbing without package identity. The
// activation factory lookup queries for IStartupTaskStatics, so it checks that
// IID against the real runtime. GetAsync is then expected to fail (no package,
// so no startup task), and the point is that it fails with an error rather
// than crashing. Nothing here can enable or disable anything.
func TestLiveWinRTProbeUnpackaged(t *testing.T) {
	requireLive(t)
	_, err := winrtCall(func() (struct{}, error) {
		f, err := factory(classStartupTask, iidStartupTaskStatics)
		if err != nil {
			return struct{}{}, err
		}
		f.release()
		return struct{}{}, nil
	})
	if err != nil {
		t.Fatalf("activation factory for %s as IStartupTaskStatics: %v", classStartupTask, err)
	}

	s, err := taskState()
	if err != nil {
		t.Logf("taskState() without package identity failed cleanly, as expected: %v", err)
		return
	}
	t.Logf("taskState() without package identity unexpectedly answered %s", s)
}
