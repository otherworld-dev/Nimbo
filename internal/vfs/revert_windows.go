//go:build windows

package vfs

// Leaving virtual-files mode: the mirror of adopt. Every placeholder under the
// root is put back the way live mode expects it — hydrated placeholders are
// REVERTED in place (data kept, placeholder metadata removed), dehydrated ones
// are DELETED (they hold no bytes; live sync re-downloads them through its
// normal pipeline, unless they sit under a path live sync ignores, in which
// case they stay on the server only). Plain files are untouched. The pass must run while the
// sync root is STILL REGISTERED (CfRevertPlaceholder needs it) — i.e. before
// Unmount — the mirror of adopt's mutations-before-watcher rule.

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/otherworld/nimbo/internal/cfapi"
)

// Seams (see the cfapi seams at the top of watcher_windows.go).
var (
	cfRevertPlaceholder = cfapi.RevertPlaceholder
	// cfIsPlaceholder reports whether fi/full names ANY placeholder (hydrated
	// or not): a reparse point in a sync tree is a placeholder — symlinks are
	// not synced. Attribute-only, so it never blocks.
	cfIsPlaceholder = func(fi os.FileInfo, full string) bool {
		d, ok := fi.Sys().(*syscall.Win32FileAttributeData)
		return ok && d.FileAttributes&windowsFileAttributeReparsePoint != 0
	}
	// cfIsDehydrated reports whether a placeholder is an online-only stub with
	// no bytes on disk. Attribute-only.
	cfIsDehydrated = func(fi os.FileInfo, _ string) bool { return cfapi.IsDehydrated(fi) }
)

const windowsFileAttributeReparsePoint = 0x00000400

// RevertPlan lists what leaving VFS must do, by rel path.
type RevertPlan struct {
	Hydrated   []string // placeholders with content — reverted in place
	Dehydrated []string // online-only stubs — deleted; sync re-downloads
	// Ignored are online-only stubs under a path live sync ignores (an old
	// node_modules on the server, say). On-demand mode lists them anyway, but
	// live mode never syncs them, so they are deleted like any stub and do
	// NOT come back: the server copy is left alone. Kept apart so the dialog
	// doesn't promise a download that never happens (Deck #512).
	Ignored []string
	// DownloadBytes is the Dehydrated stubs' total logical size: what live
	// sync will re-download after the switch. Shown to the user before
	// confirming. Ignored stubs are not in it.
	DownloadBytes int64
}

// RevertResult is what Run actually did.
type RevertResult struct {
	Reverted int
	Deleted  int
	Skipped  int // changed/vanished since the scan
	Failed   int
}

// ScanRevert classifies everything under localDir. Read-only and local-only
// (attributes, no server, no cfapi calls) — effectively instant even on huge
// trees, so it can run inline before the confirm dialog.
//
// skip (optional) is the ignore predicate live sync will apply after the
// switch (root-relative forward-slash paths). It only sorts online-only stubs
// into Ignored instead of Dehydrated. Ignored trees are still walked, because
// their stubs must be deleted all the same: left behind, they would be stubs
// with no provider once the root is unregistered.
func ScanRevert(localDir string, skip func(rel string) bool) (RevertPlan, error) {
	var plan RevertPlan
	err := filepath.WalkDir(localDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == localDir {
				return err
			}
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if path == localDir || d.IsDir() {
			return nil // dirs revert implicitly with the root's unregistration
		}
		rel, rerr := filepath.Rel(localDir, path)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		fi, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if !cfIsPlaceholder(fi, filepath.ToSlash(path)) {
			return nil // plain file — live mode already understands it
		}
		if cfIsDehydrated(fi, path) {
			if skip != nil && skip(rel) {
				plan.Ignored = append(plan.Ignored, rel)
				return nil
			}
			plan.Dehydrated = append(plan.Dehydrated, rel)
			plan.DownloadBytes += fi.Size()
		} else {
			plan.Hydrated = append(plan.Hydrated, rel)
		}
		return nil
	})
	if err != nil {
		return RevertPlan{}, err
	}
	return plan, nil
}

// Total is how many entries Run works through.
func (p RevertPlan) Total() int { return len(p.Hydrated) + len(p.Dehydrated) + len(p.Ignored) }

// Run executes the plan: revert hydrated, delete every stub. progress gets
// (done, total) per entry; ctx cancellation stops at the next entry (reverted
// files are correct plain files, the rest wait for a re-run). Entries are
// independent — one failure never strands the rest.
func (p RevertPlan) Run(ctx context.Context, localDir string, progress func(done, total int)) RevertResult {
	var res RevertResult
	total := p.Total()
	done := 0
	step := func() bool { // returns false when cancelled
		if ctx != nil && ctx.Err() != nil {
			return false
		}
		done++
		if progress != nil {
			progress(done, total)
		}
		return true
	}
	for _, rel := range p.Hydrated {
		if !step() {
			return res
		}
		if err := cfRevertPlaceholder(filepath.Join(localDir, filepath.FromSlash(rel))); err != nil {
			res.Failed++
			continue
		}
		res.Reverted++
	}
	stubs := append(append([]string(nil), p.Dehydrated...), p.Ignored...)
	for _, rel := range stubs {
		if !step() {
			return res
		}
		// A stub holds no content; deleting it cannot lose data. Live sync
		// restores it from the server as a real file, or, under an ignored
		// path, leaves it on the server only.
		if err := os.Remove(filepath.Join(localDir, filepath.FromSlash(rel))); err != nil && !os.IsNotExist(err) {
			res.Failed++
			continue
		}
		res.Deleted++
	}
	return res
}
