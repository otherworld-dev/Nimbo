package main

// Leaving virtual files (VFS -> live): the mirror of adopt.go's flow. Scan is
// instant and local (placeholder attributes only), the confirm dialog forecasts
// the work (files converted in place, online-only stubs re-downloaded by live
// sync, except those under ignored paths, which live sync never fetches), and
// the revert runs on a goroutine with a progress overlay. Ordering
// constraint, mirroring adopt's: the watcher is closed FIRST (its job is
// pushing local changes to the server — it must not see the stub deletes), the
// revert runs while the sync root is STILL registered (CfRevertPlaceholder
// needs it), and only then does the normal live switch tear the mount down and
// restore the remembered sync pairs.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/otherworld/nimbo/internal/brand"
	"github.com/otherworld/nimbo/internal/config"
	"github.com/otherworld/nimbo/internal/engine"
	"github.com/otherworld/nimbo/internal/notify"
	"github.com/otherworld/nimbo/internal/vfs"
)

type revertPending struct {
	plan vfs.RevertPlan
	dir  string
}

type revertSummary struct {
	Hydrated      int    `json:"hydrated"`      // converted back in place
	Dehydrated    int    `json:"dehydrated"`    // deleted; live sync re-downloads
	Ignored       int    `json:"ignored"`       // deleted; ignored by live sync, so stays on the server only
	DownloadBytes int64  `json:"downloadBytes"` // cost of the re-download
	Dir           string `json:"dir"`
	Error         string `json:"error,omitempty"`
}

// revertSkip is the ignore predicate live sync will apply to dir once the
// switch restores its sync folders: the global rules everywhere, plus each
// remembered folder's own excludes inside that folder, matched against the
// folder-relative path the way the engine's ignoreFor does. rel is relative to
// dir, forward slashes.
func revertSkip(dir string, global []string, remembered []config.SyncPair) func(rel string) bool {
	type scoped struct {
		local string
		ig    *engine.Ignore
	}
	var pairs []scoped
	for _, p := range remembered {
		if pathWithin(p.LocalDir, dir) {
			pairs = append(pairs, scoped{filepath.Clean(p.LocalDir),
				engine.NewIgnore(append(append([]string{}, global...), p.Excludes...))})
		}
	}
	all := engine.NewIgnore(global)
	return func(rel string) bool {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		for _, p := range pairs {
			if pathWithin(abs, p.local) {
				if r, err := filepath.Rel(p.local, abs); err == nil {
					return p.ig.Match(filepath.ToSlash(r))
				}
			}
		}
		return all.Match(rel)
	}
}

// revertIgnore builds revertSkip for this account.
func (a *App) revertIgnore(dir string) func(rel string) bool {
	global, _ := a.eng.GlobalIgnore()
	return revertSkip(dir, global, a.eng.RememberedPairs())
}

func revertError(msg string) string {
	b, _ := json.Marshal(revertSummary{Error: msg})
	return string(b)
}

// scanRevert classifies the mounted folder's placeholder states. Local-only
// and read-only — instant even on huge trees.
func (a *App) scanRevert() string {
	if a.eng == nil {
		return revertError("Not signed in.")
	}
	dir := a.GetBaseDir()
	if _, mounted := a.onDemandMounts[dir]; !mounted {
		// Nothing mounted: a plain switch is all that's needed.
		return revertError("")
	}
	plan, err := vfs.ScanRevert(dir, a.revertIgnore(dir))
	if err != nil {
		slog.Warn("revert scan", "dir", dir, "err", err)
		return revertError("Couldn't read the folder: " + err.Error())
	}
	slog.Info("revert scan", "hydrated", len(plan.Hydrated), "dehydrated", len(plan.Dehydrated),
		"ignored", len(plan.Ignored), "downloadBytes", plan.DownloadBytes, "dir", dir)
	a.pendingRevert = &revertPending{plan: plan, dir: dir}
	b, _ := json.Marshal(revertSummary{
		Hydrated:      len(plan.Hydrated),
		Dehydrated:    len(plan.Dehydrated),
		Ignored:       len(plan.Ignored),
		DownloadBytes: plan.DownloadBytes,
		Dir:           dir,
	})
	return string(b)
}

// revertAndSwitch runs the confirmed revert in the background, then completes
// the switch to live. Returns immediately; progress rides Diagnostics.
func (a *App) revertAndSwitch() string {
	if a.eng == nil {
		return "Not signed in."
	}
	pending := a.pendingRevert
	a.pendingRevert = nil
	dir := a.GetBaseDir()
	m, mounted := a.onDemandMounts[dir]
	// Mark BEFORE switching either way: the first live pass must not read an
	// unpopulated folder as a user deletion (Deck #571).
	if d, derr := config.Resolve(); derr == nil {
		_ = d.MarkPostRevert(dir)
	}
	if pending == nil || pending.dir != dir || !mounted {
		// Nothing to revert (or stale plan): plain switch.
		return a.SetSyncMode("live")
	}
	cctx, cancel := context.WithCancel(a.ctx)
	m.convertCancel = cancel
	// The watcher must not see the revert's stub deletes as user deletes — and
	// closing it FIRST also stops reconcile creating new placeholders, which is
	// why the pass re-scans below: the dialog's plan goes stale within minutes
	// on a live mount (reconcile pulled ~1.8k new stubs while one sat open).
	if m.watcher != nil {
		m.watcher.Close()
		m.watcher = nil
	}
	slog.Info("leaving virtual files: reverting placeholders in the background", "dir", dir)
	go a.runRevert(cctx, dir)
	return ""
}

func (a *App) runRevert(ctx context.Context, dir string) {
	// Fresh plan, taken AFTER the watcher stopped: complete by construction.
	// The dialog's numbers were a forecast; this is the real work list.
	plan, err := vfs.ScanRevert(dir, a.revertIgnore(dir))
	if err != nil {
		slog.Warn("revert rescan failed; remounting", "dir", dir, "err", err)
		if msg := a.SetSyncMode("ondemand"); msg != "" {
			slog.Warn("remount after failed revert rescan", "err", msg)
		}
		return
	}
	a.revertTotal.Store(int64(plan.Total()))
	a.revertDone.Store(0)
	defer func() {
		a.revertTotal.Store(0)
		a.revertDone.Store(0)
	}()
	res := plan.Run(ctx, dir, func(done, _ int) { a.revertDone.Store(int64(done)) })
	cancelled := ctx.Err() != nil
	slog.Info("revert finished", "reverted", res.Reverted, "stubsDeleted", res.Deleted,
		"failed", res.Failed, "cancelled", cancelled)
	if cancelled {
		// Put virtual files back together: remount so the remaining
		// placeholders keep working. Already-reverted files re-adopt on a
		// future switch — nothing is lost either way.
		if msg := a.SetSyncMode("ondemand"); msg != "" {
			slog.Warn("remount after cancelled revert", "err", msg)
		}
		a.emit("activity")
		return
	}
	if msg := a.SetSyncMode("live"); msg != "" {
		// Surface it: the user asked for live and didn't get it.
		slog.Warn("switch to live after revert", "err", msg)
	} else if a.NotificationsEnabled() {
		// Explicit completion: the click was minutes ago on a big account.
		notify.Toast(brand.Current.Name, fmt.Sprintf(
			"Back to normal files — %s converted in place; sync folders restored.",
			thousands(res.Reverted)), "")
	}
	a.emit("activity")
}
