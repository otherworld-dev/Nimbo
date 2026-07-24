package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/otherworld/nimbo/internal/transport"
)

// fakeServer serves canned depth-1 PROPFIND responses keyed by directory path,
// records every call, and can fail specific directories.
type fakeServer struct {
	mu    sync.Mutex
	dirs  map[string][]transport.Entry
	fail  map[string]error
	calls []string
}

func (f *fakeServer) PropFind(_ context.Context, path string, _ int) ([]transport.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, path)
	if err := f.fail[path]; err != nil {
		return nil, err
	}
	entries, ok := f.dirs[path]
	if !ok {
		return nil, fmt.Errorf("path %q not found", path)
	}
	return entries, nil
}

func (f *fakeServer) callsFor(dir string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == dir {
			n++
		}
	}
	return n
}

// d/fi build entries with permission strings sampled from a live Nextcloud:
// plain dir RGDNVCK, file RGDNVW (mounted dirs, e.g. .Collectives, are "MG").
func d(path, etag string) transport.Entry {
	return transport.Entry{Path: path, IsDir: true, ETag: etag, Permissions: "RGDNVCK"}
}
func fi(path, etag string, size int64) transport.Entry {
	return transport.Entry{Path: path, IsDir: false, ETag: etag, Size: size, Permissions: "RGDNVW"}
}

// newFakeTree builds: root -> a/ (ea), f1 ; a -> a/b/ (eb), a/f2 ; a/b -> a/b/f3.
// Depth-1 responses include the directory's own row first, as the server does.
func newFakeTree() *fakeServer {
	return &fakeServer{
		dirs: map[string][]transport.Entry{
			"":    {d("", "eroot"), d("a", "ea"), fi("f1", "e1", 1)},
			"a":   {d("a", "ea"), d("a/b", "eb"), fi("a/f2", "e2", 2)},
			"a/b": {d("a/b", "eb"), fi("a/b/f3", "e3", 3)},
		},
		fail: map[string]error{},
	}
}

func TestRemoteScanFullCrawl(t *testing.T) {
	f := newFakeTree()
	out, err := RemoteScan(context.Background(), f, "", ScanOpts{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		isDir bool
		etag  string
	}{
		"a": {true, "ea"}, "f1": {false, "e1"},
		"a/b": {true, "eb"}, "a/f2": {false, "e2"}, "a/b/f3": {false, "e3"},
	}
	if len(out) != len(want) {
		t.Fatalf("got %d entries, want %d: %v", len(out), len(want), out)
	}
	for p, w := range want {
		r, ok := out[p]
		if !ok || r.IsDir != w.isDir || r.ETag != w.etag {
			t.Errorf("out[%q] = %+v, want isDir=%v etag=%q", p, r, w.isDir, w.etag)
		}
	}
}

func TestRemoteScanBaselinePrune(t *testing.T) {
	f := newFakeTree()
	base := map[string]BaselineState{
		"a/b":    {Path: "a/b", IsDir: true, RemoteETag: "eb", RemoteFileID: "40"},
		"a/b/f3": {Path: "a/b/f3", RemoteETag: "e3", RemoteFileID: "41", LocalSize: 3},
	}
	out, err := RemoteScan(context.Background(), f, "", ScanOpts{Base: base})
	if err != nil {
		t.Fatal(err)
	}
	if f.callsFor("a/b") != 0 {
		t.Errorf("a/b was PROPFINDed despite matching baseline etag")
	}
	if r, ok := out["a/b/f3"]; !ok || r.ETag != "e3" {
		t.Errorf("pruned subtree not reconstructed from baseline: %+v", out["a/b/f3"])
	}
}

func TestRemoteScanSkipPrunesDescent(t *testing.T) {
	f := newFakeTree()
	out, err := RemoteScan(context.Background(), f, "", ScanOpts{
		Skip: func(rel string) bool { return rel == "a" },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := out["a"]; ok {
		t.Error("skipped dir recorded")
	}
	if f.callsFor("a") != 0 {
		t.Error("skipped dir was descended into")
	}
	if _, ok := out["f1"]; !ok {
		t.Error("sibling of skipped dir missing")
	}
}

func TestRemoteScanEncryptedSkipped(t *testing.T) {
	f := newFakeTree()
	f.dirs[""] = append(f.dirs[""], transport.Entry{
		Path: "vault", IsDir: true, ETag: "ev", Permissions: "RGDNVCK", IsEncrypted: true,
	})
	var enc []string
	out, err := RemoteScan(context.Background(), f, "", ScanOpts{
		OnEncrypted: func(rel string) { enc = append(enc, rel) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := out["vault"]; ok {
		t.Error("E2EE folder recorded")
	}
	if f.callsFor("vault") != 0 {
		t.Error("E2EE folder descended into")
	}
	if len(enc) != 1 || enc[0] != "vault" {
		t.Errorf("onEncrypted = %v, want [vault]", enc)
	}
}

func TestRemoteScanErrorDiscardsAll(t *testing.T) {
	f := newFakeTree()
	f.fail["a/b"] = errors.New("boom")
	out, err := RemoteScan(context.Background(), f, "", ScanOpts{})
	if err == nil {
		t.Fatal("want error")
	}
	if out != nil {
		t.Fatalf("partial result leaked: %v", out)
	}
}

// fakeCheckpoint implements ScanCheckpoint in memory, recording activity.
type cpRow struct {
	etag     string
	children []transport.Entry
}

type fakeCheckpoint struct {
	mu    sync.Mutex
	rows  map[string]cpRow
	loads []string
	saves []string
}

func newFakeCheckpoint() *fakeCheckpoint { return &fakeCheckpoint{rows: map[string]cpRow{}} }

func (f *fakeCheckpoint) Load(dir, expectedETag string) ([]transport.Entry, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads = append(f.loads, dir)
	r, ok := f.rows[dir]
	if !ok || expectedETag == "" || r.etag != expectedETag {
		return nil, false
	}
	return r.children, true
}

func (f *fakeCheckpoint) Save(dir, etag string, children []transport.Entry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saves = append(f.saves, dir)
	f.rows[dir] = cpRow{etag: etag, children: children}
}

func (f *fakeCheckpoint) touched(list []string, dir string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range list {
		if d == dir {
			return true
		}
	}
	return false
}

// scan is a shorthand for a checkpointed scan of the whole fake root.
func scan(t *testing.T, f *fakeServer, cp *fakeCheckpoint) map[string]RemoteState {
	t.Helper()
	out, err := RemoteScan(context.Background(), f, "", ScanOpts{Checkpoint: cp})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestScanWarmResumeSkipsCachedDirs(t *testing.T) {
	f := newFakeTree()
	cp := newFakeCheckpoint()
	first := scan(t, f, cp) // pre-warm: saves a and a/b (root never saved)
	if cp.touched(cp.saves, "") {
		t.Fatal("root must never be saved (queued with no parent-reported etag)")
	}
	if !cp.touched(cp.saves, "a") || !cp.touched(cp.saves, "a/b") {
		t.Fatalf("expected a and a/b saved, got %v", cp.saves)
	}
	second := scan(t, f, cp)
	if f.callsFor("") != 2 {
		t.Errorf("root fetched %d times, want 2 (always fresh)", f.callsFor(""))
	}
	if f.callsFor("a") != 1 || f.callsFor("a/b") != 1 {
		t.Errorf("cached dirs re-fetched: a=%d a/b=%d, want 1 each", f.callsFor("a"), f.callsFor("a/b"))
	}
	if len(first) != len(second) {
		t.Fatalf("cached scan differs: %v vs %v", first, second)
	}
	for k, v := range first {
		if second[k] != v {
			t.Errorf("cached scan differs at %q: %+v vs %+v", k, v, second[k])
		}
	}
}

func TestScanResumeAfterFailure(t *testing.T) {
	f := newFakeTree()
	f.dirs[""] = append(f.dirs[""], d("c", "ec"))
	f.dirs["c"] = []transport.Entry{d("c", "ec"), fi("c/f4", "e4", 4)}
	f.fail["c"] = errors.New("server buckled")
	cp := newFakeCheckpoint()
	if _, err := RemoteScan(context.Background(), f, "", ScanOpts{Checkpoint: cp}); err == nil {
		t.Fatal("want error from failed dir")
	}
	delete(f.fail, "c")
	callsBefore := map[string]int{}
	cp.mu.Lock()
	cached := make([]string, 0, len(cp.rows))
	for dir := range cp.rows {
		cached = append(cached, dir)
	}
	cp.mu.Unlock()
	for _, dir := range cached {
		callsBefore[dir] = f.callsFor(dir)
	}
	out := scan(t, f, cp)
	// Whatever the failing scan managed to save is not re-fetched on resume.
	for _, dir := range cached {
		if f.callsFor(dir) != callsBefore[dir] {
			t.Errorf("cached dir %q re-fetched on resume", dir)
		}
	}
	if _, ok := out["c/f4"]; !ok {
		t.Error("resumed scan missing the previously-failed subtree")
	}
}

func TestScanRefetchesChangedDir(t *testing.T) {
	f := newFakeTree()
	cp := newFakeCheckpoint()
	scan(t, f, cp) // pre-warm
	// Server-side change: a gets a new etag and a new child.
	f.dirs[""] = []transport.Entry{d("", "eroot2"), d("a", "ea2"), fi("f1", "e1", 1)}
	f.dirs["a"] = []transport.Entry{d("a", "ea2"), d("a/b", "eb"), fi("a/f2", "e2", 2), fi("a/f9", "e9", 9)}
	out := scan(t, f, cp)
	if f.callsFor("a") != 2 {
		t.Errorf("changed dir a fetched %d times total, want 2 (etag mismatch = miss)", f.callsFor("a"))
	}
	if f.callsFor("a/b") != 1 {
		t.Errorf("unchanged a/b re-fetched (%d calls)", f.callsFor("a/b"))
	}
	if _, ok := out["a/f9"]; !ok {
		t.Error("new file in changed dir missing")
	}
	cp.mu.Lock()
	got := cp.rows["a"].etag
	cp.mu.Unlock()
	if got != "ea2" {
		t.Errorf("row for a not overwritten: etag %q, want ea2", got)
	}
}

func TestScanEmptyETagNeverCached(t *testing.T) {
	f := newFakeTree()
	f.dirs[""] = []transport.Entry{d("", "eroot"), d("a", ""), fi("f1", "e1", 1)}
	f.dirs["a"] = []transport.Entry{d("a", ""), fi("a/f2", "e2", 2)}
	delete(f.dirs, "a/b")
	cp := newFakeCheckpoint()
	scan(t, f, cp)
	if cp.touched(cp.loads, "a") || cp.touched(cp.saves, "a") {
		t.Errorf("etag-less dir touched the checkpoint: loads=%v saves=%v", cp.loads, cp.saves)
	}
}

func TestScanMountSubtreeNeverCached(t *testing.T) {
	f := newFakeTree()
	f.dirs[""] = append(f.dirs[""], transport.Entry{Path: "m", IsDir: true, ETag: "em", Permissions: "MG"})
	f.dirs["m"] = []transport.Entry{
		{Path: "m", IsDir: true, ETag: "em", Permissions: "MG"},
		d("m/sub", "es"), // plain perms below the mount point — still excluded by inheritance
	}
	f.dirs["m/sub"] = []transport.Entry{d("m/sub", "es"), fi("m/sub/f5", "e5", 5)}
	cp := newFakeCheckpoint()
	scan(t, f, cp)
	scan(t, f, cp)
	for _, dir := range []string{"m", "m/sub"} {
		if cp.touched(cp.loads, dir) || cp.touched(cp.saves, dir) {
			t.Errorf("mount subtree dir %q touched the checkpoint", dir)
		}
		if f.callsFor(dir) != 2 {
			t.Errorf("mount subtree dir %q fetched %d times, want 2 (always fresh)", dir, f.callsFor(dir))
		}
	}
}

func TestScanLoosenedSkipFetchesFresh(t *testing.T) {
	f := newFakeTree()
	cp := newFakeCheckpoint()
	_, err := RemoteScan(context.Background(), f, "", ScanOpts{
		Checkpoint: cp,
		Skip:       func(rel string) bool { return rel == "a" },
	})
	if err != nil {
		t.Fatal(err)
	}
	if cp.touched(cp.saves, "a") {
		t.Fatal("skipped dir was cached")
	}
	out := scan(t, f, cp) // rules loosened: no Skip
	if _, ok := out["a/f2"]; !ok {
		t.Error("previously-skipped subtree missing after rules loosened")
	}
	if f.callsFor("a") != 1 {
		t.Errorf("previously-skipped dir fetched %d times, want exactly 1 (fresh on the second scan)", f.callsFor("a"))
	}
}

func TestScanCachedSelfRowTolerated(t *testing.T) {
	f := newFakeTree()
	fresh, err := RemoteScan(context.Background(), f, "", ScanOpts{})
	if err != nil {
		t.Fatal(err)
	}
	cp := newFakeCheckpoint()
	// Seed a row that (wrongly) still contains the dir's own self row.
	cp.rows["a"] = cpRow{etag: "ea", children: f.dirs["a"]}
	cp.rows["a/b"] = cpRow{etag: "eb", children: f.dirs["a/b"]}
	out := scan(t, f, cp)
	if len(out) != len(fresh) {
		t.Fatalf("self-row replay diverged: %v vs %v", out, fresh)
	}
	for k, v := range fresh {
		if out[k] != v {
			t.Errorf("self-row replay differs at %q", k)
		}
	}
}

func TestChildrenOnly(t *testing.T) {
	entries := []transport.Entry{d("a", "ea"), d("a/b", "eb"), fi("a/f2", "e2", 2)}
	got := childrenOnly(entries, "a")
	if len(got) != 2 || got[0].Path != "a/b" || got[1].Path != "a/f2" {
		t.Fatalf("childrenOnly = %v", got)
	}
	// Self row absent (transport doc: included "when present") — unchanged.
	if got2 := childrenOnly(got, "a"); len(got2) != 2 {
		t.Fatalf("childrenOnly without self row = %v", got2)
	}
}
