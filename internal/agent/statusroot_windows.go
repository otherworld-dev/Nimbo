//go:build windows

package agent

import (
	"log/slog"
	"path/filepath"
	"sync"

	"github.com/otherworld/nimbo/internal/cfapi"
)

// Status roots: a live folder registered as a STATUS-ONLY cloud sync root
// (always fully hydrated; files stay plain) so Explorer shows the Status
// column, fed per-item by NCOverlays.dll's CustomStateHandler over the status
// pipe (shell/windows/overlays/propsource.inc.cpp).
//
// v0.1.0.245-247 auto-registered every live pair this way, and it WORKS —
// rendered ticks on plain files, every channel, no elevation. It was then
// retired by product verdict (2026-08-20): the root also brings a nav-pane
// entry and native "sync pending" arrows on every plain item, and live
// folders should carry corner badges only — the Status column belongs to
// virtual-files mode. enable() is therefore currently uncalled but kept with
// its tests: it is the only proven no-elevation icon mechanism (the Store
// live-mode gap, #561, may yet want it), and disable() heals the installs
// that briefly registered.
//
// Two lifetime rules, both learned the hard way:
//   - Registration survives shutdown on purpose (#580: unregistering runs a
//     placeholder sweep; never do that as a side effect of exiting).
//   - The in-memory map only records what THIS process registered, so disable
//     must not gate on it — a mode switch after a restart still has to clean
//     up, or the pair's root would sit nested under the new on-demand mount.

// cfapi seams so tests can substitute the live Cloud Files API.
var (
	srNotify          = cfapi.ShellNotifyUpdated
	srUnregisterShell = cfapi.UnregisterShellSyncRoot
	srUnregister      = cfapi.UnregisterSyncRoot
)

type statusRoots struct {
	mu    sync.Mutex
	roots map[string]bool // local dir -> registered by this process
}

func newStatusRoots() *statusRoots { return &statusRoots{roots: map[string]bool{}} }

// enable registers dir for the Explorer Status column. Idempotent — both
// registrations use update semantics — so it runs at every live-mode startup
// and keeps the metadata (display name, icon, property definitions) current.
func (s *statusRoots) enable(dir, displayName, iconPath string) error {
	if s == nil || !cfapi.Supported() {
		return nil
	}
	if err := cfapi.RegisterStatusRoot(dir); err != nil {
		return err
	}
	if err := cfapi.RegisterShellSyncRoot(dir, displayName, iconPath, cfapi.ShellPolicyStatusOnly); err != nil {
		// The filter is registered but Explorer has no metadata for it; unwind
		// rather than leave a half-registered root behind.
		_ = cfapi.UnregisterSyncRoot(dir)
		return err
	}
	s.mu.Lock()
	first := !s.roots[dir]
	s.roots[dir] = true
	s.mu.Unlock()
	if first {
		slog.Info("status root registered for a live folder", "dir", dir)
	}
	return nil
}

// notifySynced pokes Explorer after a transfer so the Status column re-queries
// the CustomStateHandler for that one item (spinner -> tick). Only for dirs
// this process registered: anywhere else the poke would be a no-op SHChangeNotify
// per transferred file.
func (s *statusRoots) notifySynced(dir, rel string) {
	if s == nil {
		return // an Engine built outside NewEngineFor (tests)
	}
	s.mu.Lock()
	on := s.roots[dir]
	s.mu.Unlock()
	if !on {
		return
	}
	srNotify(filepath.Join(dir, filepath.FromSlash(rel)))
}

// disable unregisters dir's status root (pair removed, or the folder is about
// to go under an on-demand mount). Deliberately NOT gated on the in-memory map
// — see the file comment. No manual revert walk either: the OS unregister
// sweep reverts any placeholders left over from the old convert-the-folder
// design, and on the plain files of the current design it has nothing to do.
func (s *statusRoots) disable(dir string) {
	if s == nil || !cfapi.Supported() {
		return
	}
	s.mu.Lock()
	delete(s.roots, dir)
	s.mu.Unlock()
	srUnregisterShell(dir)
	// Log only when a filter registration actually existed — this also runs as
	// an every-startup heal on dirs that usually have nothing to remove.
	if err := srUnregister(dir); err == nil {
		slog.Info("status root unregistered", "dir", dir)
	}
}
