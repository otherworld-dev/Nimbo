package agent

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/otherworld/nimbo/internal/config"
	"github.com/otherworld/nimbo/internal/transport"
)

// saveSeenLocksLocked writes the locked set to disk so the next start begins
// with it (GitHub #7). Callers hold lockedMu, which keeps two passes' saves in
// the order their changes were made; the file is small and only changes when a
// lock comes or goes.
//
// An engine with no config dir (built by hand in tests) has nowhere to write.
func (e *Engine) saveSeenLocksLocked() {
	if e.dirs.Config == "" {
		return
	}
	var out []config.SeenLock
	for dir, list := range e.locked {
		for _, f := range list {
			out = append(out, config.SeenLock{
				LocalDir: dir, Path: f.Path,
				Owner: f.Owner, OwnerDisplay: f.OwnerDisplay, AppName: f.AppName,
				OwnerType: int(f.OwnerType), Since: f.Since,
				RemotePath: f.RemotePath, FileOwner: f.FileOwner,
			})
		}
	}
	if err := e.dirs.SaveSeenLocks(out); err != nil {
		slog.Warn("could not save the files in use by others", "err", err)
	}
}

// restoreSeenLocks puts back the locks other people held when Nimbo last ran.
//
// Without it the In use list starts empty, and a lock stays invisible until
// something in its folder changes, because an unchanged folder is never listed
// again. A lock that was released while Nimbo was closed still clears: UNLOCK
// changes the folder's ETag, so the folder is listed and reconcileLocked drops it.
//
// Only a file that still exists, under a folder the account still syncs, comes
// back. Anything else would never be listed again, so nothing would clear it.
func (e *Engine) restoreSeenLocks() {
	saved := e.dirs.LoadSeenLocks()
	if len(saved) == 0 {
		return
	}
	roots := e.lockRoots()
	restored := make(map[string][]LockedFile)
	n := 0
	for _, s := range saved {
		if !roots[rootKey(s.LocalDir)] || s.Path == "" {
			continue
		}
		if _, err := os.Lstat(filepath.Join(s.LocalDir, filepath.FromSlash(s.Path))); err != nil {
			continue
		}
		restored[s.LocalDir] = append(restored[s.LocalDir], LockedFile{
			Path: s.Path, LocalDir: s.LocalDir,
			Owner: s.Owner, OwnerDisplay: s.OwnerDisplay, AppName: s.AppName,
			OwnerType: transport.LockOwnerType(s.OwnerType), Since: s.Since,
			RemotePath: s.RemotePath, FileOwner: s.FileOwner,
		})
		n++
	}
	e.lockedMu.Lock()
	if e.locked == nil {
		e.locked = make(map[string][]LockedFile)
	}
	for dir, list := range restored {
		e.locked[dir] = append(e.locked[dir], list...)
	}
	if n != len(saved) {
		e.saveSeenLocksLocked()
	}
	e.lockedMu.Unlock()
	if n > 0 {
		slog.Info("restored files in use by others from the last run", "files", n, "dropped", len(saved)-n)
	}
}

// lockRoots is every local folder whose locks this account reports: the live
// sync pairs, and the on-demand folder, which has no pairs of its own.
func (e *Engine) lockRoots() map[string]bool {
	roots := map[string]bool{}
	if pairs, err := e.dirs.LoadPairs(); err == nil {
		for _, p := range pairs {
			roots[rootKey(p.LocalDir)] = true
		}
	}
	if st, err := e.dirs.LoadAccountState(); err == nil && st.OnDemandRoot != "" {
		roots[rootKey(st.OnDemandRoot)] = true
	}
	return roots
}

// rootKey compares folders the way Windows does: case-insensitively, and
// ignoring a trailing separator.
func rootKey(dir string) string {
	return strings.ToLower(strings.TrimRight(filepath.Clean(dir), `\/`))
}
