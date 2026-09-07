package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/otherworld/nimbo/internal/cfapi"
	"github.com/otherworld/nimbo/internal/config"
)

// "Start from scratch" mode switches: the user chose NOT to keep the files in
// the sync folder. Both directions share one safety contract, in this order:
//
//  1. wipe the affected pairs' baselines (Engine.ResetPairState) while the
//     engine is still up — an empty folder against a surviving baseline reads
//     as "the user deleted everything" and those deletes would propagate to
//     the server (that has happened: Deck #571);
//  2. stop everything that watches the tree;
//  3. delete the tree's contents;
//  4. delegate to the ordinary switch, which now finds a clean slate.
//
// A crash between any two steps lands in a safe state: wiped baseline + files
// still present re-adopts by content, wiped baseline + empty folder
// re-downloads. The dangerous combination (baseline present + folder empty)
// can never occur.

// freshOndemand switches live -> virtual files WITHOUT adopting: the local
// copies are deleted and the mount starts empty, so everything appears as
// online-only placeholders. Files that existed only locally are gone for good
// — the UI warns with exact counts before calling this.
func (a *App) freshOndemand() string {
	if a.eng == nil {
		return "Not signed in."
	}
	base := a.GetBaseDir()
	if err := a.resetPairStatesUnder(base); err != nil {
		return "Couldn't reset the sync state: " + err.Error()
	}
	a.pendingAdopt = nil // the scan's keep-my-files plan no longer applies
	a.forgetAllPairs()   // start fresh = the folder SETUP goes too, not just the files
	slog.Info("start fresh: stopping engine before clearing the folder")
	a.stopEngine()
	if err := deleteDirContents(base); err != nil {
		return "Couldn't clear the folder: " + err.Error()
	}
	slog.Info("start fresh: folder cleared, switching to virtual files", "dir", base)
	return a.SetSyncMode("ondemand")
}

// freshLive switches virtual -> live files WITHOUT converting: the placeholder
// tree is deleted and every file re-downloads from the server.
func (a *App) freshLive() string {
	if a.eng == nil {
		return "Not signed in."
	}
	base := a.GetBaseDir()
	if err := a.resetPairStatesUnder(base); err != nil {
		return "Couldn't reset the sync state: " + err.Error()
	}
	a.pendingRevert = nil     // the scan's convert-in-place plan no longer applies
	a.forgetRememberedPairs() // start fresh = no automatic folder restore either
	slog.Info("start fresh: unmounting before clearing the folder")
	a.unmountAllOnDemand()
	a.stopEngine()
	// The mount is disconnected, not unregistered; unregister AFTER deleting so
	// the OS unregister-sweep finds nothing to revert.
	if err := deleteDirContents(base); err != nil {
		return "Couldn't clear the folder: " + err.Error()
	}
	cfapi.UnregisterShellSyncRoot(base)
	_ = cfapi.UnregisterSyncRoot(base)
	slog.Info("start fresh: folder cleared, switching to live files", "dir", base)
	return a.SetSyncMode("live")
}

// forgetAllPairs removes every configured sync pair — config, watcher, and
// backup entry — WITHOUT remembering them for a later restore. "Start fresh"
// means the folder setup is gone too: after the switch the user picks folders
// again from a clean slate.
func (a *App) forgetAllPairs() {
	pairs, err := a.eng.Pairs()
	if err != nil {
		return
	}
	for _, p := range pairs {
		if err := a.eng.ForgetSyncFolder(p.RemoteRoot, false); err != nil {
			slog.Warn("start fresh: could not forget pair", "remote", p.RemoteRoot, "err", err)
			continue
		}
		slog.Info("start fresh: pair forgotten", "local", p.LocalDir, "remote", p.RemoteRoot)
	}
	a.forgetRememberedPairs()
}

// forgetRememberedPairs drops the switch-back restore list, so leaving
// virtual files later does not resurrect the pre-fresh folder setup.
func (a *App) forgetRememberedPairs() {
	if d, err := config.Resolve(); err == nil {
		_ = d.UpdateSettings(func(s *config.Settings) {
			if len(s.RememberedPairs) > 0 {
				slog.Info("start fresh: remembered folder setup cleared", "pairs", len(s.RememberedPairs))
			}
			s.RememberedPairs = nil
		})
	}
}

// resetPairStatesUnder wipes the baselines of every pair — current or
// remembered — whose local folder lives inside dir (the tree about to be
// deleted). Pairs outside dir keep their files, so they keep their baselines.
func (a *App) resetPairStatesUnder(dir string) error {
	seen := map[string]bool{}
	var all []struct{ local, remote string }
	if pairs, err := a.eng.Pairs(); err == nil {
		for _, p := range pairs {
			all = append(all, struct{ local, remote string }{p.LocalDir, p.RemoteRoot})
		}
	}
	if d, err := config.Resolve(); err == nil {
		if set, e := d.LoadSettings(); e == nil {
			for _, p := range set.RememberedPairs {
				all = append(all, struct{ local, remote string }{p.LocalDir, p.RemoteRoot})
			}
		}
	}
	for _, p := range all {
		if !pathWithin(p.local, dir) {
			continue
		}
		key := p.local + "\x00" + p.remote
		if seen[key] {
			continue
		}
		seen[key] = true
		if err := a.eng.ResetPairState(p.local, p.remote); err != nil {
			return err
		}
		slog.Info("start fresh: pair state reset", "local", p.local, "remote", p.remote)
	}
	return nil
}

// pathWithin reports whether p is dir or inside it.
func pathWithin(p, dir string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(p))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// deleteDirContents permanently removes everything inside dir, keeping dir
// itself. It refuses obviously-wrong targets: this is the "start from scratch"
// delete, and a misconfigured base dir must never be able to point it at a
// drive root or the user's home.
func deleteDirContents(dir string) error {
	dir = filepath.Clean(dir)
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("not an absolute path: %s", dir)
	}
	if vol := filepath.VolumeName(dir); dir == vol+string(filepath.Separator) || dir == vol {
		return fmt.Errorf("refusing to clear a drive root: %s", dir)
	}
	if home, err := os.UserHomeDir(); err == nil {
		if rel, rerr := filepath.Rel(filepath.Clean(dir), filepath.Clean(home)); rerr == nil {
			if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
				return fmt.Errorf("refusing to clear a folder containing the user profile: %s", dir)
			}
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	removed, failed := 0, 0
	for _, ent := range entries {
		p := filepath.Join(dir, ent.Name())
		if err := os.RemoveAll(p); err != nil {
			// Read-only attributes make RemoveAll fail on Windows; clear and retry.
			clearReadonlyUnder(p)
			if err = os.RemoveAll(p); err != nil {
				failed++
				slog.Warn("start fresh: could not remove", "path", p, "err", err)
				continue
			}
		}
		removed++
	}
	slog.Info("start fresh: folder contents removed", "dir", dir, "removed", removed, "failed", failed)
	if failed > 0 {
		return fmt.Errorf("%d item(s) could not be removed — close any programs using them and try again", failed)
	}
	return nil
}

// clearReadonlyUnder strips the read-only attribute from p and everything
// under it, best-effort.
func clearReadonlyUnder(p string) {
	_ = filepath.WalkDir(p, func(path string, d os.DirEntry, err error) error {
		if err == nil {
			_ = os.Chmod(path, 0o700)
		}
		return nil
	})
}
