package main

// Adopting an existing folder when switching to on-demand (virtual files).
//
// Mounting a cloud sync root over a folder that already holds files leaves those
// files outside the placeholder system, so they get no pin state and can go
// stale unnoticed. This drives internal/vfs's staged adopt: a read-only scan
// that produces a summary for the user, then — only on confirmation — the local
// mutations (vfs.Plan.Apply) run INSIDE the mount sequence, after the sync root
// is registered but before its write-back watcher starts, and the uploads
// (vfs.UploadPending) run in the background afterwards. The ordering is a
// correctness constraint, not a preference: a watcher that sees adopt's stub
// deletes or conflict renames pushes them to the server as user actions.
//
// Design: the VFS adopt-existing-folder spec (2026-07-26).

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/otherworld/nimbo/internal/atomicfile"
	"github.com/otherworld/nimbo/internal/brand"
	"github.com/otherworld/nimbo/internal/cfapi"
	"github.com/otherworld/nimbo/internal/config"
	"github.com/otherworld/nimbo/internal/engine"
	"github.com/otherworld/nimbo/internal/notify"
	"github.com/otherworld/nimbo/internal/vfs"
)

// adoptPending is a scan result pinned to the folder it was computed for —
// BaseDir can change between scan and confirm, and a plan must never be applied
// to a different folder than the one it described.
type adoptPending struct {
	plan vfs.Plan
	dir  string
}

// adoptSummary is what the confirmation dialog renders. Counts, not paths: the
// point is to convey scale and cost before the user commits.
type adoptSummary struct {
	Keep        int    `json:"keep"`        // already match the server, stay local
	Conflict    int    `json:"conflict"`    // differ; both versions kept
	Upload      int    `json:"upload"`      // not on the server yet
	Replace     int    `json:"replace"`     // another client's dead stubs
	UploadBytes int64  `json:"uploadBytes"` // total upload cost (incl. conflicted copies)
	Dir         string `json:"dir"`
	Error       string `json:"error,omitempty"`
}

func adoptError(msg string) string {
	b, _ := json.Marshal(adoptSummary{Error: msg})
	return string(b)
}

// scanAdopt classifies the account folder against the server WITHOUT changing
// anything, and returns the summary as JSON. Safe to call repeatedly; the plan
// is held so a following "ondemand-adopt" acts on exactly what was shown.
func (a *App) scanAdopt() string {
	if a.eng == nil {
		return adoptError("Not signed in.")
	}
	if !cfapi.Supported() {
		return adoptError("Virtual files aren't supported on this system.")
	}
	dir := a.GetBaseDir()
	if dir == "" {
		return adoptError("No folder is configured yet.")
	}
	// The crawl can run for minutes on a big account, so it is cancellable
	// (via "ondemand-cancel" from the scanning overlay) and publishes a live
	// directory count through Diagnostics — the overlay's proof of life.
	ctx, cancel := context.WithCancel(a.ctx)
	a.adoptScanMu.Lock()
	a.adoptScanCancel = cancel
	a.adoptScanMu.Unlock()
	a.adoptScanDirs.Store(0)
	defer func() {
		a.adoptScanMu.Lock()
		a.adoptScanCancel = nil
		a.adoptScanMu.Unlock()
		a.adoptScanDirs.Store(0)
		cancel()
	}()
	// The sync ignore rules apply to the adopt too: ignored trees are excluded
	// from the crawl, the plan, and therefore the upload bucket.
	skip := a.eng.GlobalIgnoreMatcher()
	remote, err := a.eng.RemoteTree(ctx, dir, "", skip, func(n int) { a.adoptScanDirs.Store(int64(n)) })
	if err != nil {
		// Nothing has changed either way — cancelled or unreachable, the user
		// simply stays on their current mode.
		if ctx.Err() != nil {
			slog.Info("adopt scan cancelled")
			return adoptError("Scan cancelled. Nothing has been changed.")
		}
		slog.Warn("adopt scan: remote tree", "err", err)
		return adoptError("Couldn't reach the server to check your files. Nothing has been changed.")
	}
	// Escaped names: the remote map's keys come back RAW (identities live in
	// that namespace), but classification must happen in LOCAL names — decode
	// the keys, and hand Scan the encoder so identities/upload targets carry
	// the server-side names. Where BOTH forms exist server-side (debris from
	// the pre-fix uploads), the escaped one wins: it's the name live sync uses.
	esc := a.eng.Escaper()
	if esc.Active() {
		decoded := make(map[string]engine.RemoteState, len(remote))
		for k, v := range remote {
			dk, wasEscaped := esc.Decode(k)
			if prev, dup := decoded[dk]; dup && !wasEscaped {
				_ = prev // literal duplicate of an escaped entry — keep the escaped one
				continue
			}
			decoded[dk] = v
		}
		remote = decoded
	}
	plan, err := vfs.Scan(dir, remote, skip, esc.Encode)
	if err != nil {
		slog.Warn("adopt scan", "dir", dir, "err", err)
		return adoptError("Couldn't read that folder: " + err.Error())
	}
	a.pendingAdopt = &adoptPending{plan: plan, dir: dir}
	counts := plan.Counts()
	b, _ := json.Marshal(adoptSummary{
		Keep:        counts[vfs.ActionKeep],
		Conflict:    counts[vfs.ActionConflict],
		Upload:      counts[vfs.ActionUpload],
		Replace:     counts[vfs.ActionReplace],
		UploadBytes: plan.UploadBytes(),
		Dir:         dir,
	})
	return string(b)
}

// adoptAndSwitch switches the account to on-demand with the confirmed plan
// staged for the mount sequence: mountOnDemandWith consumes preMountAdopt and
// runs Apply between registering the sync root and starting its watcher.
func (a *App) adoptAndSwitch() string {
	if a.eng == nil {
		return "Not signed in."
	}
	pending := a.pendingAdopt
	a.pendingAdopt = nil
	// Keeping the files is agreed, so a folder setup recorded for this is now
	// chosen for good.
	a.takeSetupFolder(a.eng.Account.ID)
	// This switch was scanned afresh, so an older resume record describes a
	// folder state that no longer exists; the new plan records its own.
	clearAdoptResume(a.eng.Account.ID)
	if pending != nil && pending.dir == a.GetBaseDir() && len(pending.plan.Entries) > 0 {
		a.preMountAdopt = pending
	}
	msg := a.SetSyncMode("ondemand")
	a.preMountAdopt = nil // consumed by the mount; dead if the mount failed
	return msg
}

// adoptResume is what an interrupted conversion still owes, persisted so the
// next mount of the same folder finishes it (Deck #500): the plan's Unfinished
// entries, pinned to the folder and remote root they were computed for.
type adoptResume struct {
	Dir     string      `json:"dir"`
	Root    string      `json:"root"`
	Entries []vfs.Entry `json:"entries"`
}

func adoptResumeFile(accountID string) string {
	d, err := config.Resolve()
	if err != nil {
		return ""
	}
	return d.WithAccount(accountID).VFSAdoptResumeFile()
}

// saveAdoptResume records the plan's unfinished part before Apply starts, so a
// shutdown mid-conversion leaves it on disk. Best effort: without it the
// conversion still runs, it just cannot be finished after a restart.
func saveAdoptResume(accountID, dir, root string, plan vfs.Plan) {
	path := adoptResumeFile(accountID)
	if path == "" {
		return
	}
	u := plan.Unfinished()
	if len(u.Entries) == 0 {
		clearAdoptResume(accountID)
		return
	}
	b, err := json.Marshal(adoptResume{Dir: dir, Root: root, Entries: u.Entries})
	if err == nil {
		err = atomicfile.Write(path, b, 0o644)
	}
	if err != nil {
		slog.Warn("adopt: couldn't record the conversion for resuming", "err", err)
	}
}

// loadAdoptResume returns the entries owed to this folder, or nil. A record
// for a different folder or root is stale (the account's folder was changed)
// and is dropped: a plan must never be applied to a folder it did not describe.
func loadAdoptResume(accountID, dir, root string) []vfs.Entry {
	path := adoptResumeFile(accountID)
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var r adoptResume
	if err := json.Unmarshal(b, &r); err != nil ||
		!strings.EqualFold(filepath.Clean(r.Dir), filepath.Clean(dir)) || r.Root != root {
		clearAdoptResume(accountID)
		return nil
	}
	return r.Entries
}

func clearAdoptResume(accountID string) {
	if path := adoptResumeFile(accountID); path != "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			slog.Warn("adopt: couldn't clear the resume record", "err", err)
		}
	}
}

// runAdoptConvert is the background conversion: Apply (all local mutations,
// with live progress via the adoptConvert atomics the Diagnostics DTO reports),
// then — preserving the ordering constraint — the write-back watcher, then the
// pending uploads. ctx is cancelled by unmount (mode switch away, shutdown):
// conversion stops at the next file, converted files stay converted (they are
// correct placeholders). Of the rest, reconcile heals the matching files and
// uploads the local-only ones; the conflicts and dead stubs are persisted
// (saveAdoptResume) and finished on the next mount, with resumed set.
func (a *App) runAdoptConvert(ctx context.Context, m *odMount, plan vfs.Plan, dir, root string,
	upload func(ctx context.Context, localPath, remotePath string) error, startWatcher func() *vfs.Watcher, resumed bool) {
	a.adoptConvertTotal.Store(int64(len(plan.Entries)))
	a.adoptConvertDone.Store(0)
	defer func() {
		a.adoptConvertTotal.Store(0)
		a.adoptConvertDone.Store(0)
	}()
	if !resumed {
		saveAdoptResume(m.accountID, dir, root, plan)
	}
	res := plan.Apply(ctx, dir, root, func(done, _ int) { a.adoptConvertDone.Store(int64(done)) })
	slog.Info("adopt applied", "kept", res.Kept, "stubsReplaced", res.Replaced,
		"conflictsRenamed", res.Renamed, "skipped", res.Skipped, "failed", res.Failed,
		"uploadsPending", len(res.Uploads), "cancelled", ctx.Err() != nil, "resumed", resumed)
	if ctx.Err() != nil {
		return // unmounted / switched away mid-convert; the resume record stays
	}
	// Every local mutation is done. A conflicted copy whose upload is cut short
	// below is a plain local-only file, which reconcile's rescue uploads.
	clearAdoptResume(m.accountID)
	w := startWatcher()
	if ctx.Err() != nil {
		// Unmount raced the watcher start — don't leave one running on a dead root.
		if w != nil {
			w.Close()
		}
		return
	}
	m.watcher = w
	slog.Info("on-demand mount connected", "dir", dir, "remoteRoot", root)
	if w != nil {
		w.Poke() // placeholder the replaced stubs and anything server-only
	}
	if len(res.Uploads) > 0 {
		a.runAdoptUploads(upload, dir, root, res.Uploads, w)
	}
	// The switch is only DONE here, long after the click — say so explicitly
	// (a silent finish reads as "did it even work?"). A resume is not a switch
	// the user just made, and its Kept count is always zero, so it stays quiet.
	if !resumed && a.NotificationsEnabled() {
		notify.Toast(brand.Current.Name, fmt.Sprintf(
			"Virtual files ready — %s files kept in place. Click to restart File Explorer so the sync icons show.",
			thousands(res.Kept)), "action=explorer-restart")
	}
	a.emit("activity")
}

// thousands renders n with thousands separators for user-facing counts.
func thousands(n int) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

// runAdoptUploads is phase B: uploads plus their post-upload marks, in the
// background — a large folder means real network time and the UI must not block
// on it. upload is the WATCHER'S upload op (uploadWithConflictFor), captured by
// the caller: it records the new ETag baseline after each upload, without which
// the next reconcile reads the just-uploaded file as "server changed" and
// re-downloads every one of them (seen live on the first VM test). It also
// never reads a.eng, so a sign-out mid-adopt can't nil-pointer this goroutine.
func (a *App) runAdoptUploads(upload func(ctx context.Context, localPath, remotePath string) error, dir, root string, uploads []vfs.UploadItem, w *vfs.Watcher) {
	failed := vfs.UploadPending(dir, root, uploads, vfs.AdoptOps{
		Upload: func(rel, remoteRel string) error {
			// remoteRel is the server-side (escaped) name; rel names the local file.
			remote := strings.Trim(root+"/"+remoteRel, "/")
			return upload(a.ctx, filepath.Join(dir, filepath.FromSlash(rel)), remote)
		},
		Log: func(f string, args ...any) { slog.Info("vfs", "msg", fmt.Sprintf(f, args...)) },
	})
	if failed > 0 {
		slog.Warn("adopt: uploads incomplete — files stay local as plain files; switching to virtual files again re-offers them", "failed", failed)
		if a.NotificationsEnabled() {
			notify.Toast(brand.Current.Name,
				fmt.Sprintf("%d file(s) couldn't be uploaded while taking over your folder. They're still safe on this PC — switch to virtual files again later to retry.", failed), "")
		}
	}
	if w != nil {
		w.Poke() // pick up anything the uploads changed server-side
	}
	a.emit("activity")
}
