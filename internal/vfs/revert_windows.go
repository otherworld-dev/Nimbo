//go:build windows

package vfs

// Leaving virtual-files mode: the mirror of adopt. Every placeholder under the
// root is put back the way live mode expects it — hydrated placeholders are
// REVERTED in place (data kept, placeholder metadata removed), dehydrated ones
// are DELETED (they hold no bytes; live sync re-downloads them through its
// normal pipeline). Plain files are untouched. The pass must run while the
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
)

const windowsFileAttributeReparsePoint = 0x00000400

// RevertPlan lists what leaving VFS must do, by rel path.
type RevertPlan struct {
	Hydrated   []string // placeholders with content — reverted in place
	Dehydrated []string // online-only stubs — deleted; sync re-downloads
	// DownloadBytes is the stubs' total logical size: what live sync will
	// re-download after the switch. Shown to the user before confirming.
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
func ScanRevert(localDir string) (RevertPlan, error) {
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
		if cfapi.IsDehydrated(fi) {
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

// Run executes the plan: revert hydrated, delete dehydrated. progress gets
// (done, total) per entry; ctx cancellation stops at the next entry (reverted
// files are correct plain files, the rest wait for a re-run). Entries are
// independent — one failure never strands the rest.
func (p RevertPlan) Run(ctx context.Context, localDir string, progress func(done, total int)) RevertResult {
	var res RevertResult
	total := len(p.Hydrated) + len(p.Dehydrated)
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
	for _, rel := range p.Dehydrated {
		if !step() {
			return res
		}
		// A stub holds no content; deleting it cannot lose data. Live sync
		// restores it from the server as a real file.
		if err := os.Remove(filepath.Join(localDir, filepath.FromSlash(rel))); err != nil && !os.IsNotExist(err) {
			res.Failed++
			continue
		}
		res.Deleted++
	}
	return res
}
