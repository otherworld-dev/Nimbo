//go:build windows

package vfs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/otherworld/nimbo/internal/engine"
	"github.com/otherworld/nimbo/internal/transport"
)

// recordMarks swaps cfMarkInSync for a recorder and restores it afterwards.
func recordMarks(t *testing.T) *[]string {
	t.Helper()
	var mu sync.Mutex
	marked := []string{}
	stopFinishedWatchers(t)
	orig := cfMarkInSync
	cfMarkInSync = func(path string, identity []byte) error {
		mu.Lock()
		defer mu.Unlock()
		marked = append(marked, filepath.ToSlash(path))
		return nil
	}
	t.Cleanup(func() { stopTestWatchers(t); cfMarkInSync = orig })
	// UpdateIdentity is attempted before marking a file that is already a
	// placeholder (repointing a foreign hydrated one). Test files are plain, so
	// stub the probe to say so and the repoint to fail, so tests don't depend
	// on cfapi behaviour.
	origU := cfUpdateIdentity
	cfUpdateIdentity = func(path string, identity []byte) error { return errors.New("not a placeholder") }
	origP := cfIsPlaceholderPath
	cfIsPlaceholderPath = func(string) (bool, error) { return false, nil }
	t.Cleanup(func() { stopTestWatchers(t); cfUpdateIdentity = origU; cfIsPlaceholderPath = origP })
	return &marked
}

// withAdoptWorkers sets Apply's pool size for one test.
func withAdoptWorkers(t *testing.T, n int) {
	t.Helper()
	orig := adoptWorkers
	adoptWorkers = n
	t.Cleanup(func() { adoptWorkers = orig })
}

func markedHas(marked []string, dir, rel string) bool {
	want := filepath.ToSlash(filepath.Join(dir, filepath.FromSlash(rel)))
	for _, m := range marked {
		if m == want {
			return true
		}
	}
	return false
}

// TestAdoptNeverMarksUnconfirmedFiles is the safety test for this feature.
//
// MarkInSync converts a file into a clean placeholder. reconcileDir is entitled
// to DELETE a clean placeholder that has no remote counterpart, treating it as a
// server-side deletion. So marking a file that isn't genuinely on the server
// hands it to the deleter — the one failure here that destroys user data.
func TestAdoptNeverMarksUnconfirmedFiles(t *testing.T) {
	dir := adoptTree(t,
		"onserver.txt:10:0",   // matches -> may be marked (phase A)
		"uploads-ok.txt:4:0",  // local-only, upload succeeds -> marked after upload
		"uploads-bad.txt:4:0", // local-only, upload FAILS -> must NEVER be marked
	)
	remote := map[string]engine.RemoteState{"onserver.txt": remoteFile(10, 0)}
	plan, err := Scan(dir, remote, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	marked := recordMarks(t)
	res := plan.Apply(context.Background(), dir, "", nil)
	if !markedHas(*marked, dir, "onserver.txt") {
		t.Error("a confirmed matching file should have been marked in-sync in phase A")
	}
	if markedHas(*marked, dir, "uploads-ok.txt") || markedHas(*marked, dir, "uploads-bad.txt") {
		t.Fatal("phase A marked a file the server does not have yet")
	}
	failed := UploadPending(dir, "", res.Uploads, AdoptOps{
		Upload: func(rel, remoteRel string) error {
			if rel == "uploads-bad.txt" {
				return errors.New("server rejected it")
			}
			return nil
		},
	})
	if failed != 1 {
		t.Errorf("failed = %d, want 1 (the rejected upload)", failed)
	}
	if markedHas(*marked, dir, "uploads-bad.txt") {
		t.Fatal("marked a file whose upload failed — reconcile would delete it")
	}
	if !markedHas(*marked, dir, "uploads-ok.txt") {
		t.Error("a successfully uploaded file should have been marked in-sync")
	}
}

func TestAdoptReplacesDeadStubs(t *testing.T) {
	dir := adoptTree(t, "stub.txt:10:0:stub", "real.txt:10:0")
	remote := map[string]engine.RemoteState{
		"stub.txt": remoteFile(10, 0),
		"real.txt": remoteFile(10, 0),
	}
	plan, err := Scan(dir, remote, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	marked := recordMarks(t)
	res := plan.Apply(context.Background(), dir, "", nil)
	if res.Replaced != 1 {
		t.Errorf("Replaced = %d, want 1", res.Replaced)
	}
	// The stub is deleted, leaving the entry remote-only for reconcile to
	// recreate as a Nimbo placeholder.
	if _, err := os.Stat(filepath.Join(dir, "stub.txt")); !os.IsNotExist(err) {
		t.Errorf("dead stub still present (stat err = %v)", err)
	}
	if markedHas(*marked, dir, "stub.txt") {
		t.Error("a deleted stub must never be marked in-sync")
	}
	if !markedHas(*marked, dir, "real.txt") {
		t.Error("the genuine local file should still be adopted")
	}
}

// Keep-both: the differing local copy is renamed to the conventional
// "conflicted copy" name IN PHASE A (before the write-back watcher exists, so
// the rename is never pushed to the server as a user move), then uploaded and
// marked in phase B. The original path is left free for reconcile to
// placeholder from the server — both versions survive.
func TestAdoptKeepBoth(t *testing.T) {
	dir := adoptTree(t, "differs.txt:99:0")
	remote := map[string]engine.RemoteState{"differs.txt": remoteFile(10, 0)}
	plan, err := Scan(dir, remote, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	marked := recordMarks(t)
	res := plan.Apply(context.Background(), dir, "", nil)
	if _, err := os.Stat(filepath.Join(dir, "differs.txt")); !os.IsNotExist(err) {
		t.Error("original conflicting path should have been renamed away")
	}
	if len(res.Uploads) != 1 || !strings.Contains(res.Uploads[0].Rel, "conflicted copy") {
		t.Fatalf("Uploads = %v, want one conflicted-copy path", res.Uploads)
	}
	if res.Uploads[0].RemoteRel != res.Uploads[0].Rel {
		t.Errorf("no escaping active: RemoteRel %q should equal Rel %q", res.Uploads[0].RemoteRel, res.Uploads[0].Rel)
	}
	conf := res.Uploads[0].Rel
	if fi, err := os.Stat(filepath.Join(dir, filepath.FromSlash(conf))); err != nil || fi.Size() != 99 {
		t.Fatalf("conflicted copy missing or wrong size: %v", err)
	}
	var uploaded []string
	failed := UploadPending(dir, "", res.Uploads, AdoptOps{
		Upload: func(rel, remoteRel string) error { uploaded = append(uploaded, rel); return nil },
	})
	if failed != 0 {
		t.Errorf("failed = %d, want 0", failed)
	}
	if len(uploaded) != 1 || uploaded[0] != conf {
		t.Errorf("uploaded %v, want [%s]", uploaded, conf)
	}
	if !markedHas(*marked, dir, conf) {
		t.Error("the uploaded conflicted copy must be marked in-sync (it is confirmed on the server)")
	}
	if markedHas(*marked, dir, "differs.txt") {
		t.Error("the original conflicting path must never be marked")
	}
}

// Re-verification: the plan can be stale by Apply time (the confirm dialog can
// sit open while syncing continues). Any entry whose file changed since the
// scan is skipped, not acted on — a later re-scan reclassifies it.
func TestAdoptApplySkipsChangedFiles(t *testing.T) {
	dir := adoptTree(t, "edited.txt:10:0", "stubgone.txt:10:0:stub")
	remote := map[string]engine.RemoteState{
		"edited.txt":   remoteFile(10, 0),
		"stubgone.txt": remoteFile(10, 0),
	}
	plan, err := Scan(dir, remote, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The user edits the matching file after the scan…
	full := filepath.Join(dir, "edited.txt")
	if err := os.WriteFile(full, make([]byte, 11), 0o644); err != nil {
		t.Fatal(err)
	}
	// …and the stub gets hydrated (offline attribute cleared): it now holds
	// real content, so deleting it would destroy data.
	stub := filepath.Join(dir, "stubgone.txt")
	p, _ := syscall.UTF16PtrFromString(stub)
	attrs, _ := syscall.GetFileAttributes(p)
	if err := syscall.SetFileAttributes(p, attrs&^uint32(0x00001000)); err != nil {
		t.Fatal(err)
	}
	marked := recordMarks(t)
	res := plan.Apply(context.Background(), dir, "", nil)
	if markedHas(*marked, dir, "edited.txt") {
		t.Error("a file edited after the scan must not be marked in-sync (the edit would never upload)")
	}
	if _, err := os.Stat(stub); err != nil {
		t.Error("a stub hydrated after the scan must NOT be deleted — it holds real content now")
	}
	if res.Skipped != 2 {
		t.Errorf("Skipped = %d, want 2", res.Skipped)
	}
}

// End-to-end classification through the real remote crawler, not a hand-built
// map: guards the whole chain RemoteScan -> RemoteState -> LocalMatchesRemote.
// (Regression: RemoteScan silently dropped LastModified, so every file on a
// real account classified as a conflict while the map-based tests stayed green.)
func TestAdoptScanFromRemoteScan(t *testing.T) {
	mt := time.Unix(1700000000, 0)
	dir := adoptTree(t, "match.txt:10:0", "sub/nested.txt:20:0")
	fake := &fakePropFinder{dirs: map[string][]transport.Entry{
		"": {
			{Path: "", IsDir: true, ETag: "er", Permissions: "RGDNVCK"},
			{Path: "match.txt", Size: 10, ETag: "e1", LastModified: mt, Permissions: "RGDNVW"},
			{Path: "sub", IsDir: true, ETag: "es", Permissions: "RGDNVCK"},
		},
		"sub": {
			{Path: "sub", IsDir: true, ETag: "es", Permissions: "RGDNVCK"},
			{Path: "sub/nested.txt", Size: 20, ETag: "e2", LastModified: mt, Permissions: "RGDNVW"},
		},
	}}
	remote, err := engine.RemoteScan(context.Background(), fake, "", engine.ScanOpts{})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Scan(dir, remote, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"match.txt", "sub/nested.txt"} {
		if got := actionFor(t, plan, rel); got != ActionKeep {
			t.Errorf("%s: got %v through real RemoteScan, want Keep", rel, got)
		}
	}
}

type fakePropFinder struct {
	dirs map[string][]transport.Entry
}

func (f *fakePropFinder) PropFind(_ context.Context, path string, _ int) ([]transport.Entry, error) {
	entries, ok := f.dirs[path]
	if !ok {
		return nil, errors.New("not found: " + path)
	}
	return entries, nil
}

// Apply can convert hundreds of thousands of files (minutes of work), so it
// reports progress per entry and honours cancellation: a cancelled run stops
// marking immediately, leaving the remaining files untouched plain files that
// a later re-scan re-offers.
func TestAdoptApplyProgressAndCancel(t *testing.T) {
	dir := adoptTree(t, "a.txt:1:0", "b.txt:1:0", "c.txt:1:0", "d.txt:1:0")
	remote := map[string]engine.RemoteState{
		"a.txt": remoteFile(1, 0), "b.txt": remoteFile(1, 0),
		"c.txt": remoteFile(1, 0), "d.txt": remoteFile(1, 0),
	}
	plan, err := Scan(dir, remote, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Full run: progress counts every entry up to the total.
	marked := recordMarks(t)
	var seen []int
	res := plan.Apply(context.Background(), dir, "", func(done, total int) {
		if total != 4 {
			t.Errorf("total = %d, want 4", total)
		}
		seen = append(seen, done)
	})
	if res.Kept != 4 || len(seen) != 4 || seen[3] != 4 {
		t.Fatalf("full run: kept=%d seen=%v", res.Kept, seen)
	}

	// Cancelled run: cancel fires after the 2nd entry; the run must stop there.
	// With one worker the stop is exact; TestAdoptApplyParallelCancel covers
	// the pool.
	withAdoptWorkers(t, 1)
	dir2 := adoptTree(t, "a.txt:1:0", "b.txt:1:0", "c.txt:1:0", "d.txt:1:0")
	plan2, err := Scan(dir2, remote, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	*marked = (*marked)[:0]
	ctx, cancel := context.WithCancel(context.Background())
	res2 := plan2.Apply(ctx, dir2, "", func(done, _ int) {
		if done == 2 {
			cancel()
		}
	})
	if res2.Kept != 2 {
		t.Errorf("cancelled run kept %d, want exactly 2 (stop at cancellation)", res2.Kept)
	}
	if len(*marked) != 2 {
		t.Errorf("cancelled run marked %d files, want 2: %v", len(*marked), *marked)
	}
}

// An interrupted conversion leaves two kinds of entry that nothing else will
// ever finish (Deck #500 item 2): an unrenamed conflict is a plain file that
// differs from the server, which reconcile's heal rightly declines, and a
// foreign dead stub is not ours to refresh. Matching files and uploads are NOT
// carried: reconcile's plain-file heal and local-only rescue already finish
// those, against the server's CURRENT state rather than a scan that may be
// days old. The resumed plan survives a JSON round trip (it is persisted
// across the restart) and replays safely over the half-done work: an entry the
// first run already handled is skipped, never renamed or deleted twice.
func TestAdoptResumeFinishesConflictsAndStubs(t *testing.T) {
	dir := adoptTree(t,
		"c1.txt:99:0",        // conflict, renamed before the interruption
		"c2.txt:99:0",        // conflict, not reached
		"keep.txt:10:0",      // matches: left to reconcile's heal
		"stub.txt:10:0:stub", // foreign dead stub, not reached
		"up.txt:4:0",         // local-only: left to reconcile's rescue
	)
	remote := map[string]engine.RemoteState{
		"c1.txt":   remoteFile(10, 0),
		"c2.txt":   remoteFile(10, 0),
		"keep.txt": remoteFile(10, 0),
		"stub.txt": remoteFile(10, 0),
	}
	plan, err := Scan(dir, remote, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	unfinished := plan.Unfinished()
	var rels []string
	for _, e := range unfinished.Entries {
		rels = append(rels, e.Rel)
	}
	if strings.Join(rels, ",") != "c1.txt,c2.txt,stub.txt" {
		t.Fatalf("Unfinished = %v, want [c1.txt c2.txt stub.txt]", rels)
	}
	b, err := json.Marshal(unfinished.Entries)
	if err != nil {
		t.Fatal(err)
	}

	// First run, interrupted after the first entry (c1.txt).
	marked := recordMarks(t)
	ctx, cancel := context.WithCancel(context.Background())
	first := plan.Apply(ctx, dir, "", func(done, _ int) {
		if done == 1 {
			cancel()
		}
	})
	if first.Renamed != 1 {
		t.Fatalf("interrupted run renamed %d, want 1", first.Renamed)
	}

	// Next start: replay what was persisted.
	var entries []Entry
	if err := json.Unmarshal(b, &entries); err != nil {
		t.Fatal(err)
	}
	*marked = (*marked)[:0]
	res := ResumePlan(entries, nil).Apply(context.Background(), dir, "", nil)
	if res.Renamed != 1 || res.Replaced != 1 || res.Skipped != 1 {
		t.Errorf("resume: renamed=%d replaced=%d skipped=%d, want 1/1/1", res.Renamed, res.Replaced, res.Skipped)
	}
	if len(res.Uploads) != 1 || !strings.HasPrefix(res.Uploads[0].Rel, "c2") || !strings.Contains(res.Uploads[0].Rel, "conflicted copy") {
		t.Errorf("resume uploads = %v, want one conflicted copy of c2.txt", res.Uploads)
	}
	if _, err := os.Stat(filepath.Join(dir, "stub.txt")); !os.IsNotExist(err) {
		t.Errorf("dead stub still present after resume (stat err = %v)", err)
	}
	copies, _ := filepath.Glob(filepath.Join(dir, "*conflicted copy*"))
	if len(copies) != 2 {
		t.Errorf("conflicted copies = %v, want exactly 2 (one each for c1 and c2)", copies)
	}
	if len(*marked) != 0 {
		t.Errorf("resume marked %v, want nothing (Keep and Upload are not carried)", *marked)
	}
	for _, rel := range []string{"keep.txt", "up.txt"} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("%s disturbed by the resume: %v", rel, err)
		}
	}
}

// adoptMark repoints the identity only on a file that is already a placeholder
// (Deck #510). On a plain file, nearly every file an adopt converts, the
// repoint is an open plus a CfUpdatePlaceholder bound to fail, so it is skipped
// and MarkInSync's convert sets the identity. A placeholder (another client's
// hydrated one) still gets the repoint, BEFORE the mark, or it would keep the
// foreign identity. A failed probe falls back to trying the repoint.
func TestAdoptMarkRepointsOnlyPlaceholders(t *testing.T) {
	var calls []string
	origU, origM, origP := cfUpdateIdentity, cfMarkInSync, cfIsPlaceholderPath
	t.Cleanup(func() { cfUpdateIdentity, cfMarkInSync, cfIsPlaceholderPath = origU, origM, origP })
	cfUpdateIdentity = func(path string, identity []byte) error {
		calls = append(calls, "repoint "+filepath.Base(path)+" "+string(identity))
		return nil
	}
	cfMarkInSync = func(path string, identity []byte) error {
		calls = append(calls, "mark "+filepath.Base(path)+" "+string(identity))
		return nil
	}
	cfIsPlaceholderPath = func(path string) (bool, error) {
		switch filepath.Base(path) {
		case "foreign.txt":
			return true, nil
		case "unprobed.txt":
			return false, errors.New("probe failed")
		}
		return false, nil
	}
	for _, name := range []string{"plain.txt", "foreign.txt", "unprobed.txt"} {
		calls = nil
		if err := adoptMark(filepath.Join(t.TempDir(), name), "Root", "sub/"+name); err != nil {
			t.Fatal(err)
		}
		want := []string{"mark " + name + " Root/sub/" + name}
		if name != "plain.txt" {
			want = append([]string{"repoint " + name + " Root/sub/" + name}, want...)
		}
		if strings.Join(calls, "|") != strings.Join(want, "|") {
			t.Errorf("%s: calls %v, want %v", name, calls, want)
		}
	}
}

// The Keep entries are marked by a worker pool (Deck #510). Every entry must
// still be marked exactly once, progress must still count 1..total in order
// from one caller at a time, and the rare entries (conflicts, uploads) keep
// plan order in Uploads.
func TestAdoptApplyParallel(t *testing.T) {
	withAdoptWorkers(t, 8)
	var specs []string
	remote := map[string]engine.RemoteState{}
	const n = 200
	for i := 0; i < n; i++ {
		rel := fmt.Sprintf("d%d/f%03d.txt", i%7, i)
		specs = append(specs, rel+":10:0")
		remote[rel] = remoteFile(10, 0)
	}
	specs = append(specs, "up1.txt:4:0", "conf.txt:99:0", "up2.txt:4:0")
	remote["conf.txt"] = remoteFile(10, 0)
	dir := adoptTree(t, specs...)
	plan, err := Scan(dir, remote, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	marked := recordMarks(t)
	// Hold each mark until a second one is in flight (or a short timeout), so
	// the test sees the pool actually overlap work.
	var inFlight, maxInFlight atomic.Int32
	recorder := cfMarkInSync
	cfMarkInSync = func(path string, identity []byte) error {
		cur := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			m := maxInFlight.Load()
			if cur <= m || maxInFlight.CompareAndSwap(m, cur) {
				break
			}
		}
		deadline := time.Now().Add(200 * time.Millisecond)
		for maxInFlight.Load() < 2 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		return recorder(path, identity)
	}

	var busy atomic.Bool
	var seen []int
	res := plan.Apply(context.Background(), dir, "", func(done, total int) {
		if !busy.CompareAndSwap(false, true) {
			t.Error("progress called concurrently")
			return
		}
		seen = append(seen, done)
		busy.Store(false)
	})
	if res.Kept != n || res.Failed != 0 || res.Skipped != 0 || res.Renamed != 1 {
		t.Fatalf("kept=%d failed=%d skipped=%d renamed=%d, want %d/0/0/1", res.Kept, res.Failed, res.Skipped, res.Renamed, n)
	}
	if len(*marked) != n {
		t.Fatalf("marked %d files, want %d", len(*marked), n)
	}
	once := map[string]bool{}
	for _, m := range *marked {
		if once[m] {
			t.Errorf("marked twice: %s", m)
		}
		once[m] = true
	}
	if len(seen) != n+3 {
		t.Fatalf("progress called %d times, want %d", len(seen), n+3)
	}
	for i, d := range seen {
		if d != i+1 {
			t.Fatalf("progress not monotonic: call %d reported %d", i, d)
		}
	}
	if maxInFlight.Load() < 2 {
		t.Errorf("max marks in flight = %d, want the pool to overlap them", maxInFlight.Load())
	}
	if len(res.Uploads) != 3 || !strings.HasPrefix(res.Uploads[0].Rel, "conf (conflicted copy") ||
		res.Uploads[1].Rel != "up1.txt" || res.Uploads[2].Rel != "up2.txt" {
		t.Errorf("Uploads = %v, want [conflicted copy of conf.txt, up1.txt, up2.txt] in plan order", res.Uploads)
	}
}

// Cancelling the pool stops it at the next entry boundary: only entries
// already started finish (at most one per other worker past the cancel), every
// one that finished is counted, and the rest are left as untouched plain files.
func TestAdoptApplyParallelCancel(t *testing.T) {
	const workers = 4
	withAdoptWorkers(t, workers)
	var specs []string
	remote := map[string]engine.RemoteState{}
	const n = 100
	for i := 0; i < n; i++ {
		rel := fmt.Sprintf("f%03d.txt", i)
		specs = append(specs, rel+":10:0")
		remote[rel] = remoteFile(10, 0)
	}
	dir := adoptTree(t, specs...)
	plan, err := Scan(dir, remote, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	marked := recordMarks(t)
	ctx, cancel := context.WithCancel(context.Background())
	const cancelAt = 10
	res := plan.Apply(ctx, dir, "", func(done, _ int) {
		if done == cancelAt {
			cancel()
		}
	})
	if res.Kept < cancelAt || res.Kept > cancelAt+workers-1 {
		t.Errorf("kept %d after a cancel at %d with %d workers, want %d..%d", res.Kept, cancelAt, workers, cancelAt, cancelAt+workers-1)
	}
	if len(*marked) != res.Kept {
		t.Errorf("marked %d files but reported kept=%d", len(*marked), res.Kept)
	}
}
