//go:build windows

package agent

import (
	"path/filepath"
	"testing"
)

// TestNotifySyncedFiresOnlyForRegisteredRoots: after a transfer the engine
// pokes Explorer so the Status column re-queries the CustomStateHandler for
// that one item — but only inside a dir this process registered as a status
// root. Notifying for on-demand mounts (or before registration) would be a
// per-file SHChangeNotify storm for nothing.
func TestNotifySyncedFiresOnlyForRegisteredRoots(t *testing.T) {
	var got []string
	on := srNotify
	t.Cleanup(func() { srNotify = on })
	srNotify = func(p string) { got = append(got, p) }

	s := newStatusRoots()
	dir := filepath.Join(`C:\`, "livepair")
	s.notifySynced(dir, "Docs/a.txt") // not registered: must stay silent
	if len(got) != 0 {
		t.Fatalf("notified for an unregistered dir: %v", got)
	}

	s.mu.Lock()
	s.roots[dir] = true
	s.mu.Unlock()
	s.notifySynced(dir, "Docs/a.txt")
	want := filepath.Join(dir, "Docs", "a.txt")
	if len(got) != 1 || got[0] != want {
		t.Errorf("notify = %v, want [%s]", got, want)
	}
}

// TestDisableUnregistersWithoutPriorEnable: sync-root registrations survive
// process restarts (kept across shutdown since #580), but the in-memory map
// does not. If disable gated on the map, a live→on-demand switch after any
// restart would leave the pair's status root registered underneath the new
// on-demand mount — nested sync roots, which Windows refuses.
func TestDisableUnregistersWithoutPriorEnable(t *testing.T) {
	var shell, filter []string
	oShell, oFilter := srUnregisterShell, srUnregister
	t.Cleanup(func() { srUnregisterShell, srUnregister = oShell, oFilter })
	srUnregisterShell = func(dir string) { shell = append(shell, dir) }
	srUnregister = func(dir string) error { filter = append(filter, dir); return nil }

	dir := filepath.Join(`C:\`, "livepair")
	s := newStatusRoots() // fresh process: map is empty
	s.disable(dir)
	if len(shell) != 1 || len(filter) != 1 {
		t.Errorf("disable must unregister regardless of the in-memory map (shell=%v filter=%v)", shell, filter)
	}
}
