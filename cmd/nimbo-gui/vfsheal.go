package main

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/otherworld/nimbo/internal/cfapi"
)

// State heal for on-demand mounts (Deck #576).
//
// A directory inside a mount can end up as a PLAIN directory with no cloud
// state at all — mode switches and placeholder reverts strip the reparse data,
// and nothing on the normal path ever converts an existing plain directory
// back. Explorer then shows the "sync pending" arrows on it forever, because a
// non-placeholder item inside a sync root reads as never-synced. Seen on the
// test VM 2026-08-18: every top-level folder plain (attrs 0x100010, state 0),
// while the files around them were healthy in-sync placeholders.
//
// The heal converts such directories back into in-sync placeholders via
// cfapi.MarkInSync — the exact conversion the VFS adopt path has run at
// 332k-file scale. Two gates keep it conservative:
//
//   - the directory must be KNOWN SERVER CONTENT, proven from the local etag
//     baselines (its own entry, or any baseline beneath it) — never from a
//     network guess. A local-only directory is the write-back watcher's to
//     upload, not ours to stamp "synced";
//   - it must have been quiet for a while, so nothing mid-flight is touched.
//
// Files are deliberately left alone: plain files inside a mount are either the
// watcher's upload queue or local-only artifacts (legacy sync DBs, logs), and
// stamping either as in-sync would be a lie with consequences.
const (
	healDelay   = 2 * time.Minute  // after mount; let the mount settle first
	healRepeat  = 6 * time.Hour    // then rescan occasionally
	healQuiesce = 10 * time.Minute // minimum directory inactivity
)

// healMountState runs the heal for one mount until ctx ends.
func (a *App) healMountState(ctx context.Context, localDir, remoteRoot string, etags *etagStore) {
	t := time.NewTimer(healDelay)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		n := healPlainDirs(localDir, remoteRoot, etags)
		if n > 0 {
			slog.Info("vfs state heal: converted plain directories back to cloud placeholders",
				"dir", localDir, "converted", n)
		}
		// And give directory PLACEHOLDERS that are not in sync their in-sync
		// state: older versions created every folder that way, opened or not,
		// which Explorer draws as the sync pending arrows (GitHub #17). Needs
		// quiet; validated by TestStateBitsAcrossLifecycle.
		if m := cfapi.SweepDirsInSync(localDir, healQuiesce); m > 0 {
			slog.Info("vfs state heal: marked directories in-sync", "dir", localDir, "marked", m)
		}
		// And settle pending free-up-space requests: Explorer's verb only sets
		// UNPINNED and waits for the provider to dehydrate — until then the
		// item and every ancestor wear the sync-pending arrows. The watcher
		// settles live requests; this catches ones made while we weren't
		// running.
		if p := cfapi.SweepSettlePins(localDir, healQuiesce); p > 0 {
			slog.Info("vfs state heal: settled pending free-up-space requests", "dir", localDir, "dehydrated", p)
		}
		t.Reset(healRepeat)
	}
}

// healPlainDirs walks one mount and converts qualifying plain directories.
// Returns how many it converted.
func healPlainDirs(localDir, remoteRoot string, etags *etagStore) int {
	converted := 0
	cutoff := time.Now().Add(-healQuiesce)
	root := filepath.Clean(localDir)
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() || p == root {
			return nil
		}
		ph, herr := cfapi.IsPlaceholder(p)
		if herr != nil || ph {
			return nil // already a placeholder (healthy), or unreadable
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		remote := path.Join(strings.Trim(remoteRoot, "/"), filepath.ToSlash(rel))
		if !etags.knownDir(remote) {
			return nil // not provably server content — leave it to the watcher
		}
		if info, ierr := os.Stat(p); ierr != nil || info.ModTime().After(cutoff) {
			return nil // recently active; next pass
		}
		if merr := cfapi.MarkInSync(p, []byte(remote)); merr != nil {
			slog.Debug("vfs state heal: convert failed", "path", p, "err", merr)
			return nil
		}
		slog.Info("vfs state heal: converted", "path", p, "remote", remote)
		cfapi.ShellNotifyUpdated(p) // redraw the glyph without a manual refresh
		converted++
		return nil
	})
	return converted
}
