//go:build windows

package vfs

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/otherworld/nimbo/internal/cfapi"
)

// --- test fakes -------------------------------------------------------------

// fakeCf swaps the cfapi seams for an in-memory placeholder world: every path
// is an in-sync placeholder unless listed in dirty (NeedsUpload). Created
// placeholders become real (empty) files/dirs so os.ReadDir sees them.
type fakeCf struct {
	mu        sync.Mutex
	dirty     map[string]bool // path -> NeedsUpload
	refreshed []string        // paths passed to RefreshPlaceholder
	repointed []string        // paths passed to UpdateIdentity
	marked    []string        // paths passed to MarkInSync
}

func installFakeCf(t *testing.T) *fakeCf {
	t.Helper()
	f := &fakeCf{dirty: map[string]bool{}}
	oi, oc, or, ou, om := cfInspect, cfCreatePlaceholders, cfRefreshPlaceholder, cfUpdateIdentity, cfMarkInSync
	t.Cleanup(func() {
		cfInspect, cfCreatePlaceholders, cfRefreshPlaceholder, cfUpdateIdentity, cfMarkInSync = oi, oc, or, ou, om
	})
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
		return nil
	}
	return f
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
		Upload: func(_ context.Context, local, remote string) error {
			r.mu.Lock()
			r.uploads = append(r.uploads, remote)
			r.mu.Unlock()
			r.uploaded <- remote
			return nil
		},
		Mkdir: func(_ context.Context, remote string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.mkdirs = append(r.mkdirs, remote)
			return nil
		},
		Delete: func(_ context.Context, remote string) error {
			r.mu.Lock()
			r.deletes = append(r.deletes, remote)
			r.mu.Unlock()
			r.deleted <- remote
			return nil
		},
		Move: func(_ context.Context, src, dst string) error {
			r.mu.Lock()
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
	}
	keep := []string{`doc.txt`, `sub\doc.txt`, `tmp`, `partial.part2`, `lock.txt`, `a~$b`}
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
