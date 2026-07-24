package agent

// Integration tests for the scan-checkpoint clear hooks (Deck #470): the
// wiring of clearCheckpoint/markCheckpointDirty into the real sync flow, as
// opposed to checkpoint_test.go's unit coverage of the handle itself. Each
// test drives the actual Engine paths (SyncOnce / syncRemoteDelta /
// applyPlan / RemoveSyncFolder) against fakeDAV — a minimal in-process
// Nextcloud WebDAV endpoint — with a real state DB and a real
// transport.Client, so the hooks run exactly as wired in production.
//
// Row assertions always probe presence BEFORE the clearing pass and absence
// after, with the exact (dir, etag) key: LoadScanDir's ok=false cannot
// distinguish "cleared" from "probed under the wrong etag", so an
// absence-only assertion could pass vacuously. Every clearing test also
// seeds a decoy row under a DIFFERENT pair_key and asserts it survives, so a
// clear that keys the wrong pair — or clears account-wide — fails loudly.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/otherworld/nimbo/internal/account"
	"github.com/otherworld/nimbo/internal/activity"
	"github.com/otherworld/nimbo/internal/config"
	"github.com/otherworld/nimbo/internal/engine"
	"github.com/otherworld/nimbo/internal/state"
	"github.com/otherworld/nimbo/internal/transport"
)

const davPrefix = "/remote.php/dav/files/alice"

// davNode is one remote object served by fakeDAV, keyed files-root-relative
// ("" is the root dir). Dirs carry an etag; files an etag and a body.
type davNode struct {
	isDir bool
	etag  string
	body  string
}

// fakeDAV serves just enough WebDAV for real sync passes: PROPFIND depth 1
// and infinity, GET, MKCOL. Concurrency-safe — the remote scan probes it
// from 4 workers at once. Failures are injected per path so a test can kill
// one directory's listing (mid-crawl scan failure) or one file's download.
type fakeDAV struct {
	mu      sync.Mutex
	nodes   map[string]davNode
	failPF  map[string]int // dir -> status served instead of its listing
	failGET map[string]int // file -> status served instead of its body
	pfCalls map[string]int // PROPFINDs seen per dir, failures included
}

func newFakeDAV(nodes map[string]davNode) *fakeDAV {
	return &fakeDAV{
		nodes:   nodes,
		failPF:  map[string]int{},
		failGET: map[string]int{},
		pfCalls: map[string]int{},
	}
}

func (f *fakeDAV) setNode(p string, n davNode)      { f.mu.Lock(); f.nodes[p] = n; f.mu.Unlock() }
func (f *fakeDAV) setFailPF(dir string, code int)   { f.mu.Lock(); f.failPF[dir] = code; f.mu.Unlock() }
func (f *fakeDAV) clearFailPF(dir string)           { f.mu.Lock(); delete(f.failPF, dir); f.mu.Unlock() }
func (f *fakeDAV) setFailGET(file string, code int) { f.mu.Lock(); f.failGET[file] = code; f.mu.Unlock() }
func (f *fakeDAV) clearFailGET(file string)         { f.mu.Lock(); delete(f.failGET, file); f.mu.Unlock() }

func (f *fakeDAV) pfCount(dir string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pfCalls[dir]
}

func (f *fakeDAV) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rel := strings.Trim(strings.TrimPrefix(r.URL.Path, davPrefix), "/")
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case "PROPFIND":
		f.pfCalls[rel]++
		if code, ok := f.failPF[rel]; ok {
			w.WriteHeader(code)
			return
		}
		n, ok := f.nodes[rel]
		if !ok || !n.isDir {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write([]byte(f.multistatus(rel, r.Header.Get("Depth") == "infinity")))
	case "GET":
		if code, ok := f.failGET[rel]; ok {
			w.WriteHeader(code)
			return
		}
		n, ok := f.nodes[rel]
		if !ok || n.isDir {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(n.body))
	case "MKCOL":
		w.WriteHeader(http.StatusCreated)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// multistatus renders dir's self row plus its children (all descendants when
// deep), in sorted order so crawl discovery is deterministic.
func (f *fakeDAV) multistatus(dir string, deep bool) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><d:multistatus xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns" xmlns:nc="http://nextcloud.org/ns">`)
	b.WriteString(f.row(dir, f.nodes[dir]))
	paths := make([]string, 0, len(f.nodes))
	for p := range f.nodes {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if p == dir || !davUnder(p, dir) {
			continue
		}
		if !deep && davParent(p) != dir {
			continue
		}
		b.WriteString(f.row(p, f.nodes[p]))
	}
	b.WriteString(`</d:multistatus>`)
	return b.String()
}

func (f *fakeDAV) row(p string, n davNode) string {
	href := davPrefix + "/" + p
	if n.isDir {
		if p == "" {
			href = davPrefix + "/"
		} else {
			href += "/"
		}
		return fmt.Sprintf(`<d:response><d:href>%s</d:href><d:propstat><d:status>HTTP/1.1 200 OK</d:status><d:prop><d:resourcetype><d:collection/></d:resourcetype><d:getetag>&quot;%s&quot;</d:getetag><oc:permissions>RGDNVCK</oc:permissions></d:prop></d:propstat></d:response>`, href, n.etag)
	}
	return fmt.Sprintf(`<d:response><d:href>%s</d:href><d:propstat><d:status>HTTP/1.1 200 OK</d:status><d:prop><d:resourcetype/><d:getetag>&quot;%s&quot;</d:getetag><d:getcontentlength>%d</d:getcontentlength><oc:permissions>RGDNVW</oc:permissions></d:prop></d:propstat></d:response>`, href, n.etag, len(n.body))
}

func davUnder(p, dir string) bool {
	if dir == "" {
		return p != ""
	}
	return strings.HasPrefix(p, dir+"/")
}

func davParent(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return ""
}

// newHookEngine builds the minimal Engine the sync paths need (mirrors
// NewEngineFor's literal; see statereset_test.go for the pattern) plus its
// opened state store.
func newHookEngine(t *testing.T, server string) (*Engine, *state.Store) {
	t.Helper()
	e := &Engine{
		Account:   account.Account{ID: "a"},
		dirs:      config.Dirs{Config: t.TempDir(), Data: t.TempDir()}.WithAccount("a"),
		client:    transport.New(server, "alice", "pw"),
		recorder:  activity.New(),
		blocked:   make(map[string][]engine.Blocked),
		conflicts: make(map[string][]ConflictItem),
	}
	st, err := e.getStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.closeStore)
	return e, st
}

func cpRowPresent(t *testing.T, st *state.Store, pk, dir, etag string) bool {
	t.Helper()
	_, _, ok, err := st.LoadScanDir(pk, dir, etag)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// seedDecoy plants a checkpoint row under an unrelated pair_key; checkDecoy
// asserts a clearing pass left it alone.
func seedDecoy(t *testing.T, st *state.Store) {
	t.Helper()
	if err := st.SaveScanDir("decoy-pair", "d", "e-d", 1, []byte("x")); err != nil {
		t.Fatal(err)
	}
}

func checkDecoy(t *testing.T, st *state.Store) {
	t.Helper()
	if !cpRowPresent(t, st, "decoy-pair", "d", "e-d") {
		t.Fatal("clear leaked into another pair's checkpoint rows")
	}
}

// The full Deck #231 loop through the real wiring: a crawl that dies partway
// keeps its rescue rows, the retry resumes from them instead of re-fetching,
// and the clean finish clears them (applyPlan's post-executor hook).
func TestSyncFailedScanResumesThenClearOnClean(t *testing.T) {
	f := newFakeDAV(map[string]davNode{
		"":       {isDir: true, etag: "e-root"},
		"a":      {isDir: true, etag: "e-a"},
		"a/b":    {isDir: true, etag: "e-b"},
		"f1":     {etag: "e-f1", body: "one"},
		"a/f2":   {etag: "e-f2", body: "two"},
		"a/b/f3": {etag: "e-f3", body: "three"},
	})
	srv := httptest.NewServer(f)
	defer srv.Close()
	e, st := newHookEngine(t, srv.URL)
	local := t.TempDir()
	p := Pair{LocalDir: local}
	pk := PairKey(local, "")
	if err := st.SetCloneStatus(pk, "done"); err != nil {
		t.Fatal(err)
	}
	seedDecoy(t, st)

	// Pass 1: a/b's listing dies (403 fails fast — no 5xx retry). a/b is only
	// discovered via a's successful listing, so a's row is guaranteed saved.
	f.setFailPF("a/b", http.StatusForbidden)
	if _, err := e.SyncOnce(context.Background(), p); err == nil {
		t.Fatal("scan with a failing dir must fail the pass")
	}
	if !cpRowPresent(t, st, pk, "a", "e-a") {
		t.Fatal("failed crawl did not keep the listed dir's checkpoint row")
	}

	// Pass 2: server healed. The cached listing must be reused, the pass must
	// finish cleanly, and the clean pass must drop every checkpoint row.
	f.clearFailPF("a/b")
	stats, err := e.SyncOnce(context.Background(), p)
	if err != nil {
		t.Fatalf("resumed pass: %v", err)
	}
	if stats.Downloaded != 3 {
		t.Fatalf("Downloaded = %d, want 3", stats.Downloaded)
	}
	if got := f.pfCount("a"); got != 1 {
		t.Fatalf("dir a PROPFINDed %d times, want 1 (resume must reuse the cached listing)", got)
	}
	for rel, want := range map[string]string{"f1": "one", "a/f2": "two", "a/b/f3": "three"} {
		got, rerr := os.ReadFile(filepath.Join(local, filepath.FromSlash(rel)))
		if rerr != nil || string(got) != want {
			t.Fatalf("%s = %q, %v — want %q", rel, got, rerr, want)
		}
	}
	if cpRowPresent(t, st, pk, "a", "e-a") || cpRowPresent(t, st, pk, "a/b", "e-b") {
		t.Fatal("clean pass did not clear the checkpoint rows")
	}
	checkDecoy(t, st)
}

// A pass whose plan is empty (everything already in sync) is still a clean
// pass and must clear rows a previous crawl left behind (applyPlan's
// zero-action hook). Uses a non-empty RemoteRoot so the pair_key the hook
// clears provably includes it — a decoy under PairKey(local, "") must survive.
func TestSyncQuietPassClearsCheckpoint(t *testing.T) {
	f := newFakeDAV(map[string]davNode{
		"":       {isDir: true, etag: "e-root"},
		"sub":    {isDir: true, etag: "e-sub"},
		"sub/f1": {etag: "e-f1", body: "one"},
	})
	srv := httptest.NewServer(f)
	defer srv.Close()
	e, st := newHookEngine(t, srv.URL)
	local := t.TempDir()
	pk := PairKey(local, "sub")
	if err := st.SetCloneStatus(pk, "done"); err != nil {
		t.Fatal(err)
	}
	// Local f1 in sync: baseline matches the server etag and the on-disk stat.
	abs := filepath.Join(local, "f1")
	if err := os.WriteFile(abs, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertBaseline(pk, engine.BaselineState{
		Path: "f1", RemoteETag: "e-f1",
		LocalSize: fi.Size(), LocalMTimeNanos: fi.ModTime().UnixNano(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveScanDir(pk, "sub/stale", "e-stale", 1, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if !cpRowPresent(t, st, pk, "sub/stale", "e-stale") {
		t.Fatal("seed row missing")
	}
	if err := st.SaveScanDir(PairKey(local, ""), "d", "e-d", 1, []byte("x")); err != nil {
		t.Fatal(err)
	}

	stats, err := e.SyncOnce(context.Background(), Pair{LocalDir: local, RemoteRoot: "sub"})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Downloaded != 0 || stats.Failed != 0 {
		t.Fatalf("expected a quiet pass, got %+v", stats)
	}
	if cpRowPresent(t, st, pk, "sub/stale", "e-stale") {
		t.Fatal("quiet pass did not clear the checkpoint")
	}
	if !cpRowPresent(t, st, PairKey(local, ""), "d", "e-d") {
		t.Fatal("clear used a pair_key that ignores RemoteRoot")
	}
}

// A quiet delta (scan sees zero divergence from the baseline) is a clean pass
// too — syncRemoteDelta's own clear hook, upstream of applyPlan.
func TestQuietDeltaClearsCheckpoint(t *testing.T) {
	f := newFakeDAV(map[string]davNode{
		"":   {isDir: true, etag: "e-root"},
		"f1": {etag: "e-f1", body: "one"},
	})
	srv := httptest.NewServer(f)
	defer srv.Close()
	e, st := newHookEngine(t, srv.URL)
	local := t.TempDir()
	pk := PairKey(local, "")
	if err := st.SetCloneStatus(pk, "done"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertBaseline(pk, engine.BaselineState{Path: "f1", RemoteETag: "e-f1"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveScanDir(pk, "stale", "e-stale", 1, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if !cpRowPresent(t, st, pk, "stale", "e-stale") {
		t.Fatal("seed row missing")
	}
	seedDecoy(t, st)

	if _, err := e.syncRemoteDelta(context.Background(), Pair{LocalDir: local}); err != nil {
		t.Fatal(err)
	}
	if cpRowPresent(t, st, pk, "stale", "e-stale") {
		t.Fatal("quiet delta did not clear the checkpoint")
	}
	checkDecoy(t, st)
}

// A pass with a failed action is NOT clean: the rescue rows must survive so
// the retry can chain off them — and the retry's clean finish clears them.
func TestFailedActionKeepsCheckpointUntilCleanPass(t *testing.T) {
	f := newFakeDAV(map[string]davNode{
		"":     {isDir: true, etag: "e-root"},
		"a":    {isDir: true, etag: "e-a"},
		"a/f2": {etag: "e-f2", body: "two"},
		"a/f3": {etag: "e-f3", body: "three"},
	})
	srv := httptest.NewServer(f)
	defer srv.Close()
	e, st := newHookEngine(t, srv.URL)
	local := t.TempDir()
	p := Pair{LocalDir: local}
	pk := PairKey(local, "")
	if err := st.SetCloneStatus(pk, "done"); err != nil {
		t.Fatal(err)
	}
	seedDecoy(t, st)

	// Pass 1: f2's download 404s (the executor's download retries burn ~1.5s
	// here — the price of driving the real path). Run reports the failure via
	// problems, not an error, so SyncOnce succeeds with Failed=1.
	f.setFailGET("a/f2", http.StatusNotFound)
	stats, err := e.SyncOnce(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Failed != 1 || stats.Downloaded != 1 {
		t.Fatalf("stats = %+v, want Failed=1 Downloaded=1", stats)
	}
	if !cpRowPresent(t, st, pk, "a", "e-a") {
		t.Fatal("pass with a failed action cleared the checkpoint")
	}

	// Pass 2: healed. The cached listing serves the rescan, only the failed
	// file re-downloads, and the now-clean pass clears the rows.
	f.clearFailGET("a/f2")
	stats, err = e.SyncOnce(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Failed != 0 || stats.Downloaded != 1 {
		t.Fatalf("retry stats = %+v, want Failed=0 Downloaded=1", stats)
	}
	if got := f.pfCount("a"); got != 1 {
		t.Fatalf("dir a PROPFINDed %d times, want 1 (retry must reuse the cached listing)", got)
	}
	if cpRowPresent(t, st, pk, "a", "e-a") {
		t.Fatal("clean retry did not clear the checkpoint")
	}
	checkDecoy(t, st)
}

// Entering a clone drops any checkpoint rows from a pre-clone life of the
// pair (cloneRemote's entry hook) — there is no baseline worth chaining
// rescue rows to.
func TestCloneEntryClearsCheckpoint(t *testing.T) {
	f := newFakeDAV(map[string]davNode{
		"": {isDir: true, etag: "e-root"}, // empty root: the clone completes with no transfers
	})
	srv := httptest.NewServer(f)
	defer srv.Close()
	e, st := newHookEngine(t, srv.URL)
	local := t.TempDir()
	pk := PairKey(local, "")
	if err := st.SaveScanDir(pk, "old", "e-old", 1, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if !cpRowPresent(t, st, pk, "old", "e-old") {
		t.Fatal("seed row missing")
	}
	seedDecoy(t, st)

	// Fresh pair (no clone status, empty baseline) -> SyncOnce enters cloneRemote.
	if _, err := e.SyncOnce(context.Background(), Pair{LocalDir: local}); err != nil {
		t.Fatal(err)
	}
	if status, _ := st.CloneStatus(pk); status != "done" {
		t.Fatalf("clone status = %q, want done", status)
	}
	if cpRowPresent(t, st, pk, "old", "e-old") {
		t.Fatal("clone entry did not clear the checkpoint")
	}
	checkDecoy(t, st)
}

// Removing a sync folder drops the pair's checkpoint rows — nothing else ever
// targets that pair_key again, and the cached listings are the big blobs.
func TestRemoveSyncFolderClearsCheckpoint(t *testing.T) {
	e, st := newHookEngine(t, "http://unused.invalid") // no client traffic on this path
	local := t.TempDir()
	if err := e.dirs.SavePairs([]config.SyncPair{{LocalDir: local, RemoteRoot: "Photos"}}); err != nil {
		t.Fatal(err)
	}
	pk := PairKey(local, "Photos")
	if err := st.SaveScanDir(pk, "Photos/dir", "e-d", 1, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if !cpRowPresent(t, st, pk, "Photos/dir", "e-d") {
		t.Fatal("seed row missing")
	}
	seedDecoy(t, st)

	if err := e.RemoveSyncFolder("Photos", false); err != nil {
		t.Fatal(err)
	}
	if cpRowPresent(t, st, pk, "Photos/dir", "e-d") {
		t.Fatal("removing the folder did not clear its checkpoint")
	}
	checkDecoy(t, st)
}

// A scan whose Saves ran must re-dirty the per-process clean hint
// (markCheckpointDirty), or a pair once marked clean would suppress every
// later clear in the same process life and its rows would linger for the
// age-out. Sequence on ONE Engine: quiet clean pass (hint goes clean) ->
// server change (next scan fetches a dir fresh and SAVES it) -> that same
// pass ends clean, and only the re-dirtied hint lets its clear actually run.
func TestScanSaveRedirtiesCleanHint(t *testing.T) {
	f := newFakeDAV(map[string]davNode{
		"":     {isDir: true, etag: "e-root"},
		"a":    {isDir: true, etag: "e-a1"},
		"a/f2": {etag: "e-f2", body: "two"},
	})
	srv := httptest.NewServer(f)
	defer srv.Close()
	e, st := newHookEngine(t, srv.URL)
	local := t.TempDir()
	p := Pair{LocalDir: local}
	pk := PairKey(local, "")
	if err := st.SetCloneStatus(pk, "done"); err != nil {
		t.Fatal(err)
	}
	// Everything in sync, dir a's baseline etag matching so pass 1 prunes it.
	if err := os.MkdirAll(filepath.Join(local, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(local, "a", "f2")
	if err := os.WriteFile(abs, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []engine.BaselineState{
		{Path: "a", IsDir: true, RemoteETag: "e-a1"},
		{Path: "a/f2", RemoteETag: "e-f2", LocalSize: fi.Size(), LocalMTimeNanos: fi.ModTime().UnixNano()},
	} {
		if err := st.UpsertBaseline(pk, b); err != nil {
			t.Fatal(err)
		}
	}

	// Pass 1: quiet and clean — marks the pair clean in the process hint.
	stats, err := e.SyncOnce(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Downloaded != 0 || stats.Failed != 0 {
		t.Fatalf("expected a quiet first pass, got %+v", stats)
	}

	// Server change: a gains f4 (new etag). Pass 2 fetches a fresh, saves its
	// listing, downloads f4, and ends clean — the save must have re-dirtied
	// the hint or this pass's clear is skipped and the row lingers.
	f.setNode("a", davNode{isDir: true, etag: "e-a2"})
	f.setNode("a/f4", davNode{etag: "e-f4", body: "four"})
	stats, err = e.SyncOnce(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Downloaded != 1 || stats.Failed != 0 {
		t.Fatalf("second pass stats = %+v, want Downloaded=1", stats)
	}
	if cpRowPresent(t, st, pk, "a", "e-a2") {
		t.Fatal("scan-saved row survived the clean finish (markCheckpointDirty wiring broken)")
	}
}

// applyPlan with base == nil (a scoped/delta merge, not a full-scan pass) must
// NOT clear — only a full reconcile's evidence covers the whole checkpoint.
// This pins the contract the SyncScope clear-trap note relies on.
func TestApplyPlanNilBaseKeepsCheckpoint(t *testing.T) {
	e, st := newHookEngine(t, "http://unused.invalid") // zero actions: no client traffic
	local := t.TempDir()
	pk := PairKey(local, "")
	if err := st.SaveScanDir(pk, "keep", "e-k", 1, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if !cpRowPresent(t, st, pk, "keep", "e-k") {
		t.Fatal("seed row missing")
	}

	if _, err := e.applyPlan(context.Background(), st, Pair{LocalDir: local}, nil,
		map[string]engine.RemoteState{}, nil, false); err != nil {
		t.Fatal(err)
	}
	if !cpRowPresent(t, st, pk, "keep", "e-k") {
		t.Fatal("nil-base pass cleared the checkpoint (scoped/delta evidence must not clear)")
	}
}
