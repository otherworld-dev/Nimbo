//go:build windows

package vfs

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"golang.org/x/sys/windows"

	"github.com/otherworld/nimbo/internal/cfapi"
)

// --- test fakes -------------------------------------------------------------

// fakeCf swaps the cfapi seams for an in-memory placeholder world: every path
// is an in-sync placeholder unless listed in dirty (NeedsUpload). Created
// placeholders become real (empty) files/dirs so os.ReadDir sees them.
type fakeCf struct {
	mu         sync.Mutex
	dirty      map[string]bool   // path -> NeedsUpload
	refreshed  []string          // paths passed to RefreshPlaceholder
	repointed  []string          // paths passed to UpdateIdentity
	marked     []string          // paths passed to MarkInSync
	identities map[string]string // path -> identity stamped by MarkInSync
	created    []string          // names passed to CreatePlaceholders
	plain      map[string]bool   // paths that are NOT placeholders (flattened)
	notified   []string          // paths passed to the shell change-notify seam
	settleChecked []string       // paths offered to the pin-settle seam
	excluded      []string       // paths passed to the exclude-from-sync seam
}

// createdNames returns the names reconcile asked to be created. Asserting on
// this rather than on the filesystem matters: NTFS is case-insensitive, so
// os.Stat("photos") succeeds when "Photos" exists and a filesystem check would
// silently pass for the wrong reason.
func (f *fakeCf) createdNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.created...)
}

func installFakeCf(t *testing.T) *fakeCf {
	t.Helper()
	f := &fakeCf{dirty: map[string]bool{}, identities: map[string]string{}, plain: map[string]bool{}}
	oi, oc, or, ou, om, op, on, os2, ox := cfInspect, cfCreatePlaceholders, cfRefreshPlaceholder, cfUpdateIdentity, cfMarkInSync, cfIsPlaceholder, cfShellNotify, cfSettlePin, cfExclude
	ov := cfRefreshIfInSync
	t.Cleanup(func() {
		cfInspect, cfCreatePlaceholders, cfRefreshPlaceholder, cfUpdateIdentity, cfMarkInSync, cfIsPlaceholder, cfShellNotify, cfSettlePin, cfExclude = oi, oc, or, ou, om, op, on, os2, ox
		cfRefreshIfInSync = ov
	})
	cfRefreshIfInSync = func(path string, identity []byte, size int64, mtime time.Time) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.dirty[strings.ToLower(path)] {
			return cfapi.ErrNotInSync // verify flag: a dirty placeholder refuses the refresh
		}
		f.refreshed = append(f.refreshed, path)
		return nil
	}
	cfExclude = func(path string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.excluded = append(f.excluded, path)
		return nil
	}
	cfSettlePin = func(path string) (bool, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.settleChecked = append(f.settleChecked, path)
		return false, nil
	}
	cfShellNotify = func(path string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.notified = append(f.notified, path)
	}
	cfIsPlaceholder = func(_ os.FileInfo, full string) bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return !f.plain[strings.ToLower(filepath.ToSlash(full))]
	}
	cfInspect = func(path string) (cfapi.Change, error) {
		fi, err := os.Lstat(path)
		if err != nil {
			return cfapi.Change{}, err
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		return cfapi.Change{IsDir: fi.IsDir(), NeedsUpload: f.dirty[strings.ToLower(path)]}, nil
	}
	cfCreatePlaceholders = func(baseDir string, items []cfapi.PlaceholderInfo) error {
		f.mu.Lock()
		for _, it := range items {
			f.created = append(f.created, it.Name)
		}
		f.mu.Unlock()
		for _, it := range items {
			p := filepath.Join(baseDir, it.Name)
			if it.IsDir {
				if err := os.MkdirAll(p, 0o755); err != nil {
					return err
				}
			} else if err := os.WriteFile(p, nil, 0o644); err != nil {
				return err
			}
		}
		return nil
	}
	cfRefreshPlaceholder = func(path string, identity []byte, size int64, mtime time.Time) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.refreshed = append(f.refreshed, path)
		return nil
	}
	cfUpdateIdentity = func(path string, identity []byte) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.repointed = append(f.repointed, path)
		return nil
	}
	cfMarkInSync = func(path string, identity []byte) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.marked = append(f.marked, path)
		f.identities[strings.ToLower(path)] = string(identity)
		return nil
	}
	return f
}

// identityOf returns the identity stamped on a path by MarkInSync. The identity
// is what the OS hands back on hydration, so for a disguised file it must name
// the file as the SERVER knows it — a detail no test could observe before.
func (f *fakeCf) identityOf(path string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.identities[strings.ToLower(path)]
	return id, ok
}

// markPlain makes a path read as a plain non-placeholder file (the #580
// flattening aftermath).
func (f *fakeCf) markPlain(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plain[strings.ToLower(filepath.ToSlash(path))] = true
}

func (f *fakeCf) markDirty(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dirty[strings.ToLower(path)] = true
}

// recorder collects the server-side ops the watcher performs.
type recorder struct {
	mu        sync.Mutex
	uploads   []string
	mkdirs    []string
	deletes   []string
	moves     [][2]string
	baselines map[string]string
	fileids   map[string]string
	listCalls map[string]int
	listing   map[string][]cfapi.PlaceholderInfo
	listErr   error
	uploaded  chan string
	deleted   chan string
	moved     chan [2]string

	uploadFails int          // fail this many uploads before succeeding…
	uploadErr   error        // …with this error
	uploadGate  chan struct{} // when non-nil, Upload blocks until it closes
	deleteFails int
	deleteErr   error
	moveFails   int
	moveErr     error
	statExists  map[string]bool // remote -> exists (nil map = Stat unavailable)
	cancelled   []string        // uploads whose ctx was cancelled mid-flight
	reports     []reportRec     // Report calls (kind, path, err)
}

type reportRec struct {
	kind, path string
	err        error
}

func newRecorder() *recorder {
	return &recorder{
		baselines: map[string]string{}, fileids: map[string]string{},
		listCalls: map[string]int{}, listing: map[string][]cfapi.PlaceholderInfo{},
		uploaded: make(chan string, 16), deleted: make(chan string, 16), moved: make(chan [2]string, 16),
	}
}

func (r *recorder) ops() Ops {
	return Ops{
		Upload: func(ctx context.Context, local, remote string) error {
			r.mu.Lock()
			gate := r.uploadGate
			if r.uploadFails > 0 {
				r.uploadFails--
				err := r.uploadErr
				r.mu.Unlock()
				return err
			}
			r.uploads = append(r.uploads, remote)
			r.mu.Unlock()
			if gate != nil {
				select {
				case <-gate:
				case <-ctx.Done():
					r.mu.Lock()
					r.cancelled = append(r.cancelled, remote)
					r.mu.Unlock()
					return ctx.Err()
				}
			}
			r.uploaded <- remote
			return nil
		},
		Stat: func(remote string) (bool, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.statExists == nil {
				return false, nil
			}
			return r.statExists[remote], nil
		},
		Report: func(kind, path string, err error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.reports = append(r.reports, reportRec{kind, path, err})
		},
		Mkdir: func(_ context.Context, remote string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.mkdirs = append(r.mkdirs, remote)
			return nil
		},
		Delete: func(_ context.Context, remote string) error {
			r.mu.Lock()
			if r.deleteFails > 0 {
				r.deleteFails--
				err := r.deleteErr
				r.mu.Unlock()
				return err
			}
			r.deletes = append(r.deletes, remote)
			r.mu.Unlock()
			r.deleted <- remote
			return nil
		},
		Move: func(_ context.Context, src, dst string) error {
			r.mu.Lock()
			if r.moveFails > 0 {
				r.moveFails--
				err := r.moveErr
				r.mu.Unlock()
				return err
			}
			r.moves = append(r.moves, [2]string{src, dst})
			r.mu.Unlock()
			r.moved <- [2]string{src, dst}
			return nil
		},
		List: func(rel string) ([]cfapi.PlaceholderInfo, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.listCalls[rel]++
			if r.listErr != nil {
				return nil, r.listErr
			}
			return r.listing[rel], nil
		},
		RecordBaseline: func(remote, etag string) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.baselines[remote] = etag
		},
		Baseline: func(remote string) (string, bool) {
			r.mu.Lock()
			defer r.mu.Unlock()
			e, ok := r.baselines[remote]
			return e, ok
		},
		RecordFileID: func(remote, id string) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.fileids[remote] = id
		},
		FileID: func(remote string) (string, bool) {
			r.mu.Lock()
			defer r.mu.Unlock()
			id, ok := r.fileids[remote]
			return id, ok
		},
		DropFileID: func(remote string) {
			r.mu.Lock()
			defer r.mu.Unlock()
			delete(r.fileids, remote)
		},
	}
}

// bareWatcher builds a Watcher for direct method tests (no OS watch loop).
func bareWatcher(root string, ops Ops) *Watcher {
	ctx, cancel := context.WithCancel(context.Background())
	w := &Watcher{
		root: root, remoteRoot: "", ops: ops, ctx: ctx, cancel: cancel,
		upload: map[string]*time.Timer{}, delete: map[string]*time.Timer{},
		suppress: map[string]time.Time{},
		inflight: map[string]context.CancelFunc{}, again: map[string]bool{},
		attempts: map[string]int{}, delAttempts: map[string]int{},
		mvAttempts: map[string]int{}, busyCount: map[string]int{},
	}
	if w.ops.Log == nil {
		w.ops.Log = func(string, ...any) {}
	}
	return w
}

func ph(name string, dir bool, etag, fileid string) cfapi.PlaceholderInfo {
	return cfapi.PlaceholderInfo{
		Name: name, IsDir: dir, ModTime: time.Now(), Size: 0,
		Identity: []byte(name), ETag: etag, FileID: fileid,
	}
}

// --- pure helpers -----------------------------------------------------------

func TestSkipName(t *testing.T) {
	skip := []string{
		`doc.nimbo-part`, `sub\doc.nimbo-part`, `Thumbs.db`, `desktop.ini`, `.DS_Store`,
		`~$report.docx`, `.~lock.report.odt#`, `save.tmp`, `dl.crdownload`, `x.~tmp`,
		// Windows writes these with varying case — the comparison must not care
		// (live bug: "Desktop.ini" was adopted and conflict-copied to the server).
		`Desktop.ini`, `THUMBS.DB`, `SAVE.TMP`,
		// The official Nextcloud/ownCloud client's local artifacts: its sync DB
		// and log live inside the sync folder and are left behind on migration —
		// pure pollution, never sync them.
		`.sync_74b7ab355ec5.db`, `.sync_74b7ab355ec5.db-wal`, `.sync_74b7ab355ec5.db-shm`,
		`.sync_abc.db-journal`, `.nextcloudsync.log`, `.owncloudsync.log`,
		`sub\.sync_11ff.db`, `.Nextcloudsync.log`,
	}
	keep := []string{`doc.txt`, `sub\doc.txt`, `tmp`, `partial.part2`, `lock.txt`, `a~$b`,
		// Not artifacts: user files that merely resemble them.
		`sync.db`, `my.sync_notes.txt`, `.syncthing.txt`, `desktop.initial.png`}
	for _, n := range skip {
		if !skipName(n) {
			t.Errorf("skipName(%q) = false, want true", n)
		}
	}
	for _, n := range keep {
		if skipName(n) {
			t.Errorf("skipName(%q) = true, want false", n)
		}
	}
}

func TestRemoteFor(t *testing.T) {
	w := bareWatcher(`C:\root`, Ops{})
	w.remoteRoot = "Photos"
	if got := w.remoteFor(`C:\root\sub\a.txt`); got != "Photos/sub/a.txt" {
		t.Errorf("remoteFor nested = %q", got)
	}
	if got := w.remoteFor(`C:\root`); got != "" {
		t.Errorf("remoteFor(root) = %q, want \"\" (must never delete the root)", got)
	}
	w.remoteRoot = ""
	if got := w.remoteFor(`C:\root\a.txt`); got != "a.txt" {
		t.Errorf("remoteFor account-root = %q", got)
	}
}

func TestServerChanged(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	if !serverChanged(cfapi.PlaceholderInfo{Size: 99, ModTime: fi.ModTime()}, fi) {
		t.Error("size change not detected")
	}
	if serverChanged(cfapi.PlaceholderInfo{Size: fi.Size(), ModTime: fi.ModTime()}, fi) {
		t.Error("identical file flagged as changed")
	}
	if !serverChanged(cfapi.PlaceholderInfo{Size: fi.Size(), ModTime: fi.ModTime().Add(time.Minute)}, fi) {
		t.Error("newer mtime not detected")
	}
}

func TestRemoteChangedPrefersETag(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	rec := newRecorder()
	w := bareWatcher(dir, rec.ops())
	rec.baselines["f.txt"] = "etag-1"
	// Same size/mtime (the heuristic would say unchanged) but a new ETag → changed.
	r := cfapi.PlaceholderInfo{Size: fi.Size(), ModTime: fi.ModTime(), ETag: "etag-2"}
	if !w.remoteChanged(r, fi, "f.txt") {
		t.Error("ETag change missed (same-size edit would be left stale)")
	}
	r.ETag = "etag-1"
	if w.remoteChanged(r, fi, "f.txt") {
		t.Error("matching ETag flagged as changed")
	}
}

func TestSuppress(t *testing.T) {
	w := bareWatcher(`C:\root`, Ops{})
	w.suppressDelete(`C:\root\Sub`)
	if !w.isSuppressed(`C:\root\sub`) {
		t.Error("case-insensitive match failed")
	}
	if !w.isSuppressed(`C:\root\sub\child.txt`) {
		t.Error("subtree match failed")
	}
	if w.isSuppressed(`C:\root\sub2`) {
		t.Error("sibling prefix wrongly suppressed")
	}
}

// --- FILE_NOTIFY_INFORMATION parsing ---------------------------------------

// notifyBuf encodes FILE_NOTIFY_INFORMATION records like ReadDirectoryChanges.
func notifyBuf(t *testing.T, recs []struct {
	action uint32
	name   string
}) []byte {
	t.Helper()
	var buf []byte
	for i, r := range recs {
		u := utf16.Encode([]rune(r.name))
		rec := make([]byte, 12+len(u)*2)
		if pad := len(rec) % 4; pad != 0 {
			rec = append(rec, make([]byte, 4-pad)...)
		}
		next := uint32(0)
		if i < len(recs)-1 {
			next = uint32(len(rec))
		}
		binary.LittleEndian.PutUint32(rec[0:], next)
		binary.LittleEndian.PutUint32(rec[4:], r.action)
		binary.LittleEndian.PutUint32(rec[8:], uint32(len(u)*2))
		for j, c := range u {
			binary.LittleEndian.PutUint16(rec[12+j*2:], c)
		}
		buf = append(buf, rec...)
	}
	return buf
}

func TestParseDispatch(t *testing.T) {
	rec := newRecorder()
	w := bareWatcher(`C:\root`, rec.ops())
	defer w.cancel()

	w.parse(notifyBuf(t, []struct {
		action uint32
		name   string
	}{
		{fileActionAdded, "new.txt"},
		{fileActionModified, `sub\edit.txt`},
		{fileActionRemoved, "gone.txt"},
		{fileActionAdded, "skip.tmp"}, // must be filtered out
	}))

	w.mu.Lock()
	_, up1 := w.upload[`C:\root\new.txt`]
	_, up2 := w.upload[`C:\root\sub\edit.txt`]
	_, del := w.delete[`C:\root\gone.txt`]
	_, skipped := w.upload[`C:\root\skip.tmp`]
	for _, tm := range w.upload {
		tm.Stop()
	}
	for _, tm := range w.delete {
		tm.Stop()
	}
	w.mu.Unlock()

	if !up1 || !up2 {
		t.Error("ADDED/MODIFIED did not schedule uploads")
	}
	if !del {
		t.Error("REMOVED did not schedule a delete")
	}
	if skipped {
		t.Error("temp file (.tmp) was not filtered")
	}
}

func TestParseRenamePair(t *testing.T) {
	rec := newRecorder()
	w := bareWatcher(`C:\root`, rec.ops())
	defer w.cancel()

	w.parse(notifyBuf(t, []struct {
		action uint32
		name   string
	}{
		{fileActionRenamedOld, "old.txt"},
		{fileActionRenamedNew, "new.txt"},
	}))

	select {
	case mv := <-rec.moved:
		if mv != [2]string{"old.txt", "new.txt"} {
			t.Errorf("move = %v", mv)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("rename pair did not dispatch a Move")
	}
}

func TestDeleteCancelledWhenPathReturns(t *testing.T) {
	rec := newRecorder()
	w := bareWatcher(`C:\root`, rec.ops())
	defer w.cancel()

	// REMOVED then ADDED for the same path (atomic save / rename shuffle): the
	// pending server delete must be cancelled.
	w.parse(notifyBuf(t, []struct {
		action uint32
		name   string
	}{{fileActionRemoved, "doc.txt"}}))
	w.parse(notifyBuf(t, []struct {
		action uint32
		name   string
	}{{fileActionAdded, "doc.txt"}}))

	w.mu.Lock()
	_, stillPending := w.delete[`C:\root\doc.txt`]
	for _, tm := range w.upload {
		tm.Stop()
	}
	w.mu.Unlock()
	if stillPending {
		t.Error("delete not cancelled by the path reappearing")
	}
}

// --- reconcile (down-sync) ---------------------------------------------------

func TestReconcileCreatesAdditions(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "keep.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{
		ph("keep.txt", false, "e-keep", "f-keep"),
		ph("new.txt", false, "e-new", "f-new"),
		ph("sub", true, "e-sub", ""),
	}
	rec.baselines["keep.txt"] = "e-keep" // in sync, must not be touched
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	if _, err := os.Stat(filepath.Join(root, "new.txt")); err != nil {
		t.Error("server addition new.txt not created locally")
	}
	if fi, err := os.Stat(filepath.Join(root, "sub")); err != nil || !fi.IsDir() {
		t.Error("server addition sub/ not created locally")
	}
	if rec.baselines["new.txt"] != "e-new" {
		t.Errorf("baseline for new.txt = %q, want e-new", rec.baselines["new.txt"])
	}
	if rec.fileids["new.txt"] != "f-new" {
		t.Errorf("fileid for new.txt = %q, want f-new", rec.fileids["new.txt"])
	}
}

func TestReconcilePropagatesServerDelete(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	gone := filepath.Join(root, "gone.txt")
	dirty := filepath.Join(root, "dirty.txt")
	for _, p := range []string{gone, dirty} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.markDirty(dirty) // pending local upload — must survive

	rec := newRecorder()
	rec.listing[""] = nil // server says: nothing here
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Error("in-sync placeholder not removed on server delete")
	}
	if _, err := os.Stat(dirty); err != nil {
		t.Error("pending-upload file was wrongly deleted")
	}
}

func TestReconcileListErrorIsNotEmpty(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	keep := filepath.Join(root, "keep.txt")
	if err := os.WriteFile(keep, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := newRecorder()
	rec.listErr = os.ErrDeadlineExceeded // any listing failure
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	if _, err := os.Stat(keep); err != nil {
		t.Fatal("listing failure treated as empty directory — local file deleted")
	}
}

func TestReconcileDetectsServerRename(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	old := filepath.Join(root, "old.txt")
	if err := os.WriteFile(old, []byte("hydrated"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := newRecorder()
	rec.fileids["old.txt"] = "fid-1" // recorded when old.txt was created
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("renamed.txt", false, "e2", "fid-1")}
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	if _, err := os.Stat(filepath.Join(root, "renamed.txt")); err != nil {
		t.Fatal("renamed placeholder missing — rename fell back to delete+create")
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("old path still present after server rename")
	}
	f.mu.Lock()
	repointed := len(f.repointed) == 1
	f.mu.Unlock()
	if !repointed {
		t.Error("placeholder identity not repointed to the new remote path")
	}
	if rec.baselines["renamed.txt"] != "e2" {
		t.Error("baseline not recorded under the new path")
	}
	if _, ok := rec.fileids["old.txt"]; ok {
		t.Error("stale fileid for the old path not dropped")
	}
	if len(rec.deletes) != 0 {
		t.Error("server rename caused a spurious delete")
	}
}

func TestReconcileRefreshesChangedFile(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	doc := filepath.Join(root, "doc.txt")
	if err := os.WriteFile(doc, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := newRecorder()
	rec.baselines["doc.txt"] = "etag-v1"
	r := ph("doc.txt", false, "etag-v2", "fid") // server has a new version
	fi, _ := os.Stat(doc)
	r.Size, r.ModTime = fi.Size(), fi.ModTime() // same size/mtime: only the ETag differs
	rec.listing[""] = []cfapi.PlaceholderInfo{r}
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	f.mu.Lock()
	refreshed := len(f.refreshed) == 1
	f.mu.Unlock()
	if !refreshed {
		t.Fatal("same-size server edit not refreshed (stale local copy)")
	}
	if rec.baselines["doc.txt"] != "etag-v2" {
		t.Error("baseline not advanced to the refreshed version")
	}
}

func TestReconcileETagSubtreeSkip(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "a.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("sub", true, "etag-sub", "")}
	rec.listing["sub"] = []cfapi.PlaceholderInfo{ph("a.txt", false, "e-a", "f-a")}
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile() // pass 1: lists root + sub, records sub's collection ETag
	w.Reconcile() // pass 2: sub's ETag unchanged → its subtree must be skipped

	rec.mu.Lock()
	rootCalls, subCalls := rec.listCalls[""], rec.listCalls["sub"]
	rec.mu.Unlock()
	if rootCalls != 2 {
		t.Errorf("root listed %d times, want 2 (always re-listed)", rootCalls)
	}
	if subCalls != 1 {
		t.Errorf("sub listed %d times, want 1 (ETag subtree skip)", subCalls)
	}
}

// --- live watcher over a real directory --------------------------------------

func TestWatcherEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("debounce timing test")
	}
	f := installFakeCf(t)
	root := t.TempDir()
	rec := newRecorder()
	ops := rec.ops()
	ops.List = nil // no reconcile loop in this test
	w, err := New(context.Background(), root, "", 0, ops)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// Create: a brand-new (dirty) file must be uploaded and marked in-sync.
	p := filepath.Join(root, "a.txt")
	f.markDirty(p)
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-rec.uploaded:
		if got != "a.txt" {
			t.Fatalf("uploaded %q, want a.txt", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("create was not uploaded")
	}

	// Rename: must MOVE on the server, not delete+upload.
	if err := os.Rename(p, filepath.Join(root, "b.txt")); err != nil {
		t.Fatal(err)
	}
	select {
	case mv := <-rec.moved:
		if mv != [2]string{"a.txt", "b.txt"} {
			t.Fatalf("move = %v", mv)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("rename was not moved")
	}

	// Delete: must propagate to the server after the debounce.
	if err := os.Remove(filepath.Join(root, "b.txt")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-rec.deleted:
		if got != "b.txt" {
			t.Fatalf("deleted %q, want b.txt", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("delete was not propagated")
	}
}

// --- #580 aftermath: plain files healed by reconcile -------------------------

// healSetup builds a root with one local file the fake reports as PLAIN (no
// cloud state — the #580 flattening aftermath) and a server listing for it.
func healSetup(t *testing.T, etag, baseline string, matchMeta bool) (*fakeCf, *recorder, *Watcher, string) {
	t.Helper()
	f := installFakeCf(t)
	root := t.TempDir()
	doc := filepath.Join(root, "doc.txt")
	if err := os.WriteFile(doc, []byte("same bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markPlain(doc)

	rec := newRecorder()
	r := ph("doc.txt", false, etag, "fid-1")
	if matchMeta {
		fi, _ := os.Stat(doc)
		r.Size, r.ModTime = fi.Size(), fi.ModTime()
	} else {
		r.Size = 999 // server version demonstrably differs
	}
	rec.listing[""] = []cfapi.PlaceholderInfo{r}
	if baseline != "" {
		rec.baselines["doc.txt"] = baseline
	}
	w := bareWatcher(root, rec.ops())
	return f, rec, w, doc
}

// A flattened file whose size+mtime match the server listing, with the server
// unchanged since our last sync (baseline == listing ETag), is converted back
// to an in-sync placeholder — Explorer's perpetual "sync pending" arrows on it
// disappear without any transfer.
func TestReconcileHealsFlattenedFile(t *testing.T) {
	f, rec, w, doc := healSetup(t, "etag-v1", "etag-v1", true)
	defer w.cancel()

	w.Reconcile()

	f.mu.Lock()
	marked := append([]string(nil), f.marked...)
	f.mu.Unlock()
	if len(marked) != 1 || marked[0] != doc {
		t.Fatalf("flattened file not healed (marked=%v)", marked)
	}
	if id, _ := f.identityOf(doc); id != "doc.txt" {
		t.Errorf("healed with identity %q, want doc.txt", id)
	}
	if rec.baselines["doc.txt"] != "etag-v1" {
		t.Errorf("baseline = %q, want etag-v1", rec.baselines["doc.txt"])
	}
	if rec.fileids["doc.txt"] != "fid-1" {
		t.Errorf("fileid = %q, want fid-1", rec.fileids["doc.txt"])
	}
	if _, err := os.Stat(doc); err != nil {
		t.Error("file vanished during heal")
	}
	f.mu.Lock()
	notified := append([]string(nil), f.notified...)
	f.mu.Unlock()
	if len(notified) != 1 || notified[0] != doc {
		t.Errorf("shell not notified of the healed item (notified=%v) — Explorer keeps the stale glyph until a manual refresh", notified)
	}
}

// A plain file that does NOT match the server metadata is someone's pending
// work (a local edit the watcher never saw, or a newer server version) — it
// must not be stamped in-sync, uploaded, or deleted by the heal.
func TestReconcileLeavesMismatchedPlainFileAlone(t *testing.T) {
	f, rec, w, doc := healSetup(t, "etag-v2", "etag-v1", false)
	defer w.cancel()

	w.Reconcile()

	f.mu.Lock()
	marked := len(f.marked)
	f.mu.Unlock()
	if marked != 0 {
		t.Fatal("mismatched plain file was wrongly stamped in-sync")
	}
	rec.mu.Lock()
	ups, dels := len(rec.uploads), len(rec.deletes)
	rec.mu.Unlock()
	if ups != 0 || dels != 0 {
		t.Fatalf("heal performed server ops (uploads=%d deletes=%d)", ups, dels)
	}
	if _, err := os.Stat(doc); err != nil {
		t.Error("plain file deleted")
	}
}

// Size and mtime matching is not enough when the recorded baseline says the
// server has moved on since we last synced this file: the identical-looking
// metadata could be a coincidence, so the heal must decline.
func TestReconcileHealRespectsStaleBaseline(t *testing.T) {
	f, _, w, _ := healSetup(t, "etag-v2", "etag-v1", true)
	defer w.cancel()

	w.Reconcile()

	f.mu.Lock()
	marked := len(f.marked)
	f.mu.Unlock()
	if marked != 0 {
		t.Fatal("plain file stamped in-sync despite a stale baseline")
	}
}

// The ETag subtree skip must not starve the heal: with nothing changed
// server-side every subdirectory is skipped before its contents are listed, so
// a #580-flattened file deep in the tree would never meet healPlainFile. The
// FIRST pass after the watcher starts therefore ignores the skip; later passes
// use it as usual.
func TestReconcileFirstPassHealsInsideUnchangedSubtree(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	doc := filepath.Join(root, "sub", "doc.txt")
	if err := os.WriteFile(doc, []byte("same bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markPlain(doc)

	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("sub", true, "e-sub", "")}
	r := ph("doc.txt", false, "e-doc", "fid-1")
	r.Identity = []byte("sub/doc.txt")
	fi, _ := os.Stat(doc)
	r.Size, r.ModTime = fi.Size(), fi.ModTime()
	rec.listing["sub"] = []cfapi.PlaceholderInfo{r}
	// The subtree looks unchanged: sub's recorded collection ETag matches the
	// listing, and the file's baseline matches its ETag.
	rec.baselines["sub"] = "e-sub"
	rec.baselines["sub/doc.txt"] = "e-doc"
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	f.mu.Lock()
	marked := append([]string(nil), f.marked...)
	f.mu.Unlock()
	if len(marked) != 1 || marked[0] != doc {
		t.Fatalf("flattened file inside an unchanged subtree not healed on the first pass (marked=%v)", marked)
	}

	// A later pass goes back to skipping the unchanged subtree.
	rec.mu.Lock()
	before := rec.listCalls["sub"]
	rec.mu.Unlock()
	w.Reconcile()
	rec.mu.Lock()
	after := rec.listCalls["sub"]
	rec.mu.Unlock()
	if after != before {
		t.Errorf("second pass re-listed the unchanged subtree (%d -> %d) — ETag skip lost", before, after)
	}
}

// A change event on a file that needs no upload may still be a PIN change:
// Explorer's "Free up space" sets FILE_ATTRIBUTE_UNPINNED and waits for the
// provider to dehydrate. The watcher must offer such files to the settle path
// instead of ignoring them — otherwise the request hangs forever as
// sync-pending arrows (VM, 2026-08-19: "i just told it to free up space...
// says sync pending, where nimbo isnt syncing anything").
func TestChangeEventSettlesPendingUnpin(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	doc := filepath.Join(root, "doc.txt")
	if err := os.WriteFile(doc, []byte("uploaded already"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Clean (no upload needed) — the branch that used to just return.
	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleChange(doc)

	f.mu.Lock()
	settled := append([]string(nil), f.settleChecked...)
	f.mu.Unlock()
	if len(settled) != 1 || settled[0] != doc {
		t.Fatalf("clean file's change event not offered to the settle path (checked=%v)", settled)
	}
	rec.mu.Lock()
	ups := len(rec.uploads)
	rec.mu.Unlock()
	if ups != 0 {
		t.Fatalf("clean file wrongly uploaded (%d uploads)", ups)
	}
}

// Plain directories inside a mount (flatten victims) that are EMPTY are a
// permanent dead-end for every other heal: reconcile treats an empty dir as
// lazy-unpopulated and never lists it, so it never earns the etag baseline
// healPlainDirs demands — and Explorer reads a non-placeholder dir as
// never-synced, projecting pending arrows onto every ancestor (VM: 17 such
// dirs under Disk Info1\Smart kept the folder on arrows after everything else
// healed). Reconcile holds the server listing — proof enough: an in-both
// plain EMPTY dir is replaced with a real lazy placeholder…
func TestReconcileReplacesEmptyPlainDir(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Smart"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.markPlain(filepath.Join(root, "Smart"))
	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("Smart", true, "e-smart", "")}
	rec.listing["Smart"] = []cfapi.PlaceholderInfo{ph("data.csv", false, "e-csv", "f-csv")}
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	got := f.createdNames()
	if len(got) < 1 || got[0] != "Smart" {
		t.Fatalf("empty plain dir not recreated as a placeholder (created=%v)", got)
	}
	if fi, err := os.Stat(filepath.Join(root, "Smart")); err != nil || !fi.IsDir() {
		t.Fatalf("Smart missing after replacement: %v", err)
	}
	// The replacement must not stop at a LAZY placeholder — a not-in-sync dir
	// draws the same pending arrows the heal exists to remove. Reconcile is
	// online: populate the fresh dir from the server listing and mark it
	// in-sync, settled end to end.
	foundChild := false
	for _, n := range got {
		if n == "data.csv" {
			foundChild = true
		}
	}
	if !foundChild {
		t.Errorf("replacement did not populate the fresh dir from the listing (created=%v)", got)
	}
	f.mu.Lock()
	marked := append([]string(nil), f.marked...)
	f.mu.Unlock()
	dirMarked := false
	for _, m := range marked {
		if m == filepath.Join(root, "Smart") {
			dirMarked = true
		}
	}
	if !dirMarked {
		t.Errorf("replaced dir not marked in-sync (marked=%v) — it would wear lazy arrows until first opened", marked)
	}
	rec.mu.Lock()
	dels := len(rec.deletes)
	rec.mu.Unlock()
	if dels != 0 {
		t.Fatalf("replacement leaked a server delete (%d) — the local remove must be suppressed", dels)
	}
}

// …and an in-both plain POPULATED dir is converted in place (the adopt-proven
// conversion), keeping its contents untouched.
func TestReconcileConvertsPopulatedPlainDir(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	sub := filepath.Join(root, "Keep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "data.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markPlain(sub)
	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("Keep", true, "e-keep", "")}
	rec.listing["Keep"] = []cfapi.PlaceholderInfo{ph("data.txt", false, "e-d", "fd")}
	rec.baselines["Keep/data.txt"] = "e-d"
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	f.mu.Lock()
	marked := append([]string(nil), f.marked...)
	f.mu.Unlock()
	found := false
	for _, m := range marked {
		if m == sub {
			found = true
		}
	}
	if !found {
		t.Fatalf("populated plain dir not converted in place (marked=%v)", marked)
	}
	if b, err := os.ReadFile(filepath.Join(sub, "data.txt")); err != nil || string(b) != "x" {
		t.Fatalf("contents disturbed: %v", err)
	}
}

// --- issue #1: failed uploads must retry, invisibly-stuck files must rescue ---

// A transiently-failed upload must be retried automatically. Before this, a
// failed upload was reported once and the file ignored forever ("As if that
// file specifically is now ignored. Previous failure and no retry mechanism?"
// — GitHub issue #1).
func TestUploadFailureRetriedWithBackoff(t *testing.T) {
	ob, om := retryBase, retryMax
	retryBase, retryMax = 20*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { retryBase, retryMax = ob, om })

	f := installFakeCf(t)
	root := t.TempDir()
	doc := filepath.Join(root, "doc.txt")
	if err := os.WriteFile(doc, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDirty(doc)
	rec := newRecorder()
	rec.uploadFails = 2
	rec.uploadErr = errors.New("Put \"https://x\": http2: server sent GOAWAY")
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleChange(doc)

	select {
	case got := <-rec.uploaded:
		if got != "doc.txt" {
			t.Fatalf("uploaded %q, want doc.txt", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("failed upload never retried")
	}
	rec.mu.Lock()
	var failReports int
	for _, r := range rec.reports {
		if r.kind == "upload" && r.err != nil {
			failReports++
		}
	}
	rec.mu.Unlock()
	if failReports == 0 {
		t.Error("failures not reported to the activity feed")
	}
}

func TestRetryDelayGrowsAndCaps(t *testing.T) {
	if retryDelay(1) != retryBase {
		t.Errorf("first retry = %v, want %v", retryDelay(1), retryBase)
	}
	if retryDelay(2) != 2*retryBase {
		t.Errorf("second retry = %v, want %v", retryDelay(2), 2*retryBase)
	}
	if d := retryDelay(50); d != retryMax {
		t.Errorf("delay after many failures = %v, want capped at %v", d, retryMax)
	}
}

// Only one upload may run per path: change events landing while a (multi-hour)
// upload is in flight must not start a second concurrent upload of the same
// file — they re-arm one more pass after it finishes.
func TestInFlightUploadNotDuplicated(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	doc := filepath.Join(root, "big.bin")
	if err := os.WriteFile(doc, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDirty(doc)
	rec := newRecorder()
	gate := make(chan struct{})
	rec.uploadGate = gate
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	go w.handleChange(doc)
	// Wait for the first upload to be in flight (it blocks on the gate after
	// recording itself).
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec.mu.Lock()
		n := len(rec.uploads)
		rec.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first upload never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	w.handleChange(doc) // a change event during the upload
	rec.mu.Lock()
	n := len(rec.uploads)
	rec.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d concurrent uploads of the same path, want 1", n)
	}
	rec.mu.Lock()
	rec.uploadGate = nil
	rec.mu.Unlock()
	close(gate)
	<-rec.uploaded
	// The queued change re-arms one more upload after the first finishes.
	select {
	case <-rec.uploaded:
	case <-time.After(5 * time.Second):
		t.Fatal("change during in-flight upload was lost")
	}
}

// Reconcile must re-arm the upload of a dirty local-only file: a failed upload
// whose retry state died with the process (or whose change event was lost)
// would otherwise sit dirty forever.
func TestReconcileReschedulesDirtyLocalOnlyFile(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	doc := filepath.Join(root, "stuck.txt")
	if err := os.WriteFile(doc, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDirty(doc)
	rec := newRecorder()
	rec.listing[""] = nil // server doesn't have it
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	select {
	case got := <-rec.uploaded:
		if got != "stuck.txt" {
			t.Fatalf("uploaded %q, want stuck.txt", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dirty local-only file not rescued by reconcile")
	}
}

// The first reconcile pass after start must also rescue a dirty file that
// exists on BOTH sides (an edit whose upload failed before a restart).
func TestReconcileFirstPassRescuesDirtyInBothFile(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	doc := filepath.Join(root, "edited.txt")
	if err := os.WriteFile(doc, []byte("local edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDirty(doc)
	rec := newRecorder()
	r := ph("edited.txt", false, "e-1", "f-1")
	rec.listing[""] = []cfapi.PlaceholderInfo{r}
	rec.baselines["edited.txt"] = "e-1"
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	select {
	case got := <-rec.uploaded:
		if got != "edited.txt" {
			t.Fatalf("uploaded %q, want edited.txt", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dirty in-both file not rescued on the first pass")
	}
}

// A local sharing violation (the file is still being written — e.g. Explorer
// mid-copy of a 300GB file) is "try again soon", not a failure to report.
func TestSharingViolationRetriesQuietly(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	doc := filepath.Join(root, "copying.tif")
	if err := os.WriteFile(doc, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDirty(doc)
	rec := newRecorder()
	rec.uploadFails = 1
	rec.uploadErr = &os.PathError{Op: "open", Path: doc, Err: windows.ERROR_SHARING_VIOLATION}
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleChange(doc)

	rec.mu.Lock()
	var failReports int
	for _, r := range rec.reports {
		if r.err != nil {
			failReports++
		}
	}
	rec.mu.Unlock()
	if failReports != 0 {
		t.Error("busy-file error reported as a failure — should re-arm quietly like a lock hold")
	}
	w.mu.Lock()
	_, pending := w.upload[doc]
	w.mu.Unlock()
	if !pending {
		t.Error("busy file not rescheduled")
	}
}

// --- audit fixes: mid-upload edits, delete/rename resilience, rescue gaps ----

// An edit landing while that file's upload is in flight must NOT be stamped
// in-sync by the completing upload: the stamp would clear the dirty bit the
// edit set, the re-armed pass would see nothing to do, and the newest content
// would never upload (audit: confirmed data loss, destroyed on dehydrate).
func TestUploadDoesNotMarkInSyncOverMidUploadEdit(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	doc := filepath.Join(root, "doc.txt")
	if err := os.WriteFile(doc, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDirty(doc)
	rec := newRecorder()
	gate := make(chan struct{})
	rec.uploadGate = gate
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	done := make(chan struct{})
	go func() { w.handleChange(doc); close(done) }()
	// Wait until the upload is in flight, then edit the file under it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec.mu.Lock()
		n := len(rec.uploads)
		rec.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("upload never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // ensure a distinguishable mtime
	if err := os.WriteFile(doc, []byte("v2 - newer content"), 0o644); err != nil {
		t.Fatal(err)
	}
	close(gate)
	<-rec.uploaded
	<-done

	f.mu.Lock()
	marked := len(f.marked)
	f.mu.Unlock()
	if marked != 0 {
		t.Fatal("file edited during its upload was stamped in-sync — the edit is lost")
	}
	w.mu.Lock()
	_, rearmed := w.upload[doc]
	w.mu.Unlock()
	if !rearmed {
		t.Fatal("edited file not re-armed for another upload")
	}
}

// A transiently-failed server DELETE must retry — dropping it resurrects the
// deleted files at the next reconcile.
func TestDeleteRetriedOnTransientFailure(t *testing.T) {
	ob := retryBase
	retryBase = 20 * time.Millisecond
	t.Cleanup(func() { retryBase = ob })
	installFakeCf(t)
	root := t.TempDir()
	rec := newRecorder()
	rec.deleteFails = 1
	rec.deleteErr = errors.New("dial tcp: connection refused")
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleDelete(filepath.Join(root, "gone.txt"))

	select {
	case got := <-rec.deleted:
		if got != "gone.txt" {
			t.Fatalf("deleted %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("failed delete never retried")
	}
}

// A transient MOVE failure during a rename must retry the MOVE — the old
// fallback re-uploaded the file under its new name and left the old server
// copy behind (double storage, then a resurrect on reconcile).
func TestRenameMoveTransientFailureRetriesMove(t *testing.T) {
	ob := retryBase
	retryBase = 20 * time.Millisecond
	t.Cleanup(func() { retryBase = ob })
	installFakeCf(t)
	root := t.TempDir()
	newp := filepath.Join(root, "new.txt")
	if err := os.WriteFile(newp, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := newRecorder()
	rec.moveFails = 1
	rec.moveErr = errors.New("read tcp: connection reset by peer")
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleRename(filepath.Join(root, "old.txt"), newp)

	select {
	case mv := <-rec.moved:
		if mv != [2]string{"old.txt", "new.txt"} {
			t.Fatalf("move = %v", mv)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("transient MOVE failure never retried")
	}
	rec.mu.Lock()
	ups := len(rec.uploads)
	rec.mu.Unlock()
	if ups != 0 {
		t.Fatalf("transient MOVE failure fell back to a full upload (%d)", ups)
	}
}

// A MOVE that "fails" because the server already applied it (response lost)
// must be recognised via the destination, not degraded into an upload.
func TestRenameMoveLostResponseDetectedViaStat(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	newp := filepath.Join(root, "new.txt")
	if err := os.WriteFile(newp, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := newRecorder()
	rec.moveFails = 99
	rec.moveErr = errors.New("read tcp: connection reset by peer")
	rec.statExists = map[string]bool{"new.txt": true} // the server DID apply it
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleRename(filepath.Join(root, "old.txt"), newp)

	rec.mu.Lock()
	ups := len(rec.uploads)
	rec.mu.Unlock()
	if ups != 0 {
		t.Fatalf("lost-response rename degraded into an upload (%d)", ups)
	}
	// The placeholder identity must be repointed to the new name (success path).
	w.mu.Lock()
	pending := len(w.upload)
	w.mu.Unlock()
	if pending != 0 {
		t.Fatalf("lost-response rename left retries pending (%d)", pending)
	}
}

// A 404 on MOVE still means "never uploaded" and falls back to uploading.
func TestRenameMove404FallsBackToUpload(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	newp := filepath.Join(root, "new.txt")
	if err := os.WriteFile(newp, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDirty(newp)
	rec := newRecorder()
	rec.moveFails = 99
	rec.moveErr = errors.New(`MOVE "old.txt": server returned 404 Not Found: gone`)
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleRename(filepath.Join(root, "old.txt"), newp)

	select {
	case got := <-rec.uploaded:
		if got != "new.txt" {
			t.Fatalf("uploaded %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("404 MOVE did not fall back to upload")
	}
}

// Deleting a file while its (multi-hour) upload is in flight must cancel the
// upload — otherwise the assembly recreates the file on the server after the
// DELETE, and reconcile resurrects it locally.
func TestDeleteDuringUploadCancelsIt(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	doc := filepath.Join(root, "big.bin")
	if err := os.WriteFile(doc, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDirty(doc)
	rec := newRecorder()
	gate := make(chan struct{})
	rec.uploadGate = gate
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	go w.handleChange(doc)
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec.mu.Lock()
		n := len(rec.uploads)
		rec.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("upload never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := os.Remove(doc); err != nil {
		t.Fatal(err)
	}
	w.handleDelete(doc)

	select {
	case got := <-rec.deleted:
		if got != "big.bin" {
			t.Fatalf("deleted %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("delete never reached the server")
	}
	rec.mu.Lock()
	cancelled := len(rec.cancelled)
	rec.mu.Unlock()
	if cancelled != 1 {
		t.Fatalf("in-flight upload not cancelled by the delete (cancelled=%d)", cancelled)
	}
}

// The dirty in-both rescue must run on EVERY reconcile pass, not just the
// first: a file whose change event was lost (buffer overflow) would otherwise
// stay stuck until the next app restart.
func TestReconcileRescuesDirtyInBothOnLaterPasses(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	doc := filepath.Join(root, "edited.txt")
	if err := os.WriteFile(doc, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := newRecorder()
	r := ph("edited.txt", false, "e-1", "f-1")
	rec.listing[""] = []cfapi.PlaceholderInfo{r}
	rec.baselines["edited.txt"] = "e-1"
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile() // pass 1: clean — nothing to do
	f.markDirty(doc)
	w.Reconcile() // pass 2: the file is now dirty with no live event

	select {
	case got := <-rec.uploaded:
		if got != "edited.txt" {
			t.Fatalf("uploaded %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dirty in-both file not rescued on a later pass")
	}
}

// Sync-excluded artifacts (the official client's journals, Desktop.ini) are
// plain LOCAL-ONLY files that Explorer renders as forever-pending inside a
// cloud root. Reconcile must offer them to the exclude path (convert +
// CF_PIN_STATE_EXCLUDED -> blank Status cell, VM-verified) instead of
// skipping them into permanent arrows.
func TestReconcileExcludesSkippedLocalFiles(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	ini := filepath.Join(root, "desktop.ini")
	if err := os.WriteFile(ini, []byte("[.ShellClassInfo]"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "real.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("real.txt", false, "e-r", "f-r")}
	rec.baselines["real.txt"] = "e-r"
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	f.mu.Lock()
	excluded := append([]string(nil), f.excluded...)
	f.mu.Unlock()
	if len(excluded) != 1 || excluded[0] != ini {
		t.Fatalf("skipName'd local file not offered to the exclude path (excluded=%v)", excluded)
	}
	if _, err := os.Stat(ini); err != nil {
		t.Fatal("desktop.ini deleted — exclusion must never remove the file")
	}
	rec.mu.Lock()
	ups := len(rec.uploads)
	rec.mu.Unlock()
	if ups != 0 {
		t.Fatalf("excluded file wrongly uploaded (%d)", ups)
	}
}
