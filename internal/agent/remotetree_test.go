package agent

import (
	"context"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	"github.com/otherworld/nimbo/internal/config"
	"github.com/otherworld/nimbo/internal/engine"
)

// The adopt scan (RemoteTree) is minutes long on a big account and is exactly
// the crawl a cautious user cancels and retries — so it must be checkpoint-
// backed: a repeat scan reuses cached listings and only re-fetches what
// changed, instead of restarting cold.
func TestRemoteTreeResumesFromCheckpoint(t *testing.T) {
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
	e, _ := newHookEngine(t, srv.URL)

	// Pass 1: a/b's listing dies mid-crawl (as a cancel or network blip would).
	f.setFailPF("a/b", 403)
	if _, err := e.RemoteTree(context.Background(), "", "", nil, nil); err == nil {
		t.Fatal("scan with a failing dir must fail")
	}

	// Pass 2: healed. The already-listed dir must come from the checkpoint.
	f.clearFailPF("a/b")
	remote, err := e.RemoteTree(context.Background(), "", "", nil, nil)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got := f.pfCount("a"); got != 1 {
		t.Fatalf("dir a PROPFINDed %d times across both passes, want 1 (retry must reuse the cached listing)", got)
	}
	if _, ok := remote["a/b/f3"]; !ok {
		t.Fatal("retry missing the previously-failed subtree")
	}
	// The result must be complete and carry what adopt classification needs —
	// CRUCIALLY LastModified, on the REPLAYED entries: the checkpoint codec once
	// dropped it, so a warm re-scan classified every on-server file as a
	// conflict (live incident: 556k conflicted copies / 478 GB upload offered;
	// only the confirm dialog stopped it).
	want := time.Unix(1700000000, 0)
	for _, rel := range []string{"a/f2", "f1", "a/b/f3"} {
		r, ok := remote[rel]
		if !ok || r.ETag == "" {
			t.Fatalf("remote[%s] incomplete: %+v", rel, r)
		}
		if !r.LastModified.Equal(want) {
			t.Errorf("remote[%s].LastModified = %v, want %v (checkpoint replay must preserve mtimes)", rel, r.LastModified, want)
		}
	}
}

// adoptFixture is a server tree plus an engine whose live pair (localDir,
// root "") has a baseline recording every file in sync, as a live sync
// leaves it. mtimeNS is what that baseline says each file's local modified
// time was at the last sync (0 for a baseline without times).
func adoptFixture(t *testing.T, mtimeNS int64, excludes []string) (*fakeDAV, *Engine, string) {
	t.Helper()
	f := newFakeDAV(map[string]davNode{
		"":                 {isDir: true, etag: "e-root"},
		"a":                {isDir: true, etag: "e-a"},
		"a/b":              {isDir: true, etag: "e-b"},
		"f1":               {etag: "e-f1", body: "one"},
		"a/f2":             {etag: "e-f2", body: "two"},
		"a/b/f3":           {etag: "e-f3", body: "three"},
		"a/node_modules":   {isDir: true, etag: "e-nm"},
		"a/node_modules/x": {etag: "e-x", body: "x"},
	})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	e, st := newHookEngine(t, srv.URL)
	local := t.TempDir()
	if err := e.dirs.SavePairs([]config.SyncPair{{LocalDir: local, RemoteRoot: "", Excludes: excludes}}); err != nil {
		t.Fatal(err)
	}
	pk := PairKey(local, "")
	for _, b := range []engine.BaselineState{
		{Path: "a", IsDir: true, RemoteETag: "e-a"},
		{Path: "a/b", IsDir: true, RemoteETag: "e-b"},
		{Path: "f1", RemoteETag: "e-f1", LocalSize: 3, LocalMTimeNanos: mtimeNS},
		{Path: "a/f2", RemoteETag: "e-f2", LocalSize: 3, LocalMTimeNanos: mtimeNS},
		{Path: "a/b/f3", RemoteETag: "e-f3", LocalSize: 5, LocalMTimeNanos: mtimeNS},
		{Path: "a/node_modules", IsDir: true, RemoteETag: "e-nm"},
		{Path: "a/node_modules/x", RemoteETag: "e-x", LocalSize: 1, LocalMTimeNanos: mtimeNS},
	} {
		if err := st.UpsertBaseline(pk, b); err != nil {
			t.Fatal(err)
		}
	}
	return f, e, local
}

// Deck #500 (5). Switching a live-synced account to on-demand crawled the
// whole server even though the live baseline already knew every unchanged
// folder: minutes on a big account where the live delta scan takes seconds.
// The adopt scan now prunes a folder whose ETag the baseline holds, like the
// delta scan does.
//
// The trap is the 556k-conflict near-miss: a folder rebuilt from the baseline
// carries no server modified time, and the adopt classifier cannot call a
// file in sync without one, so every file under a pruned folder would have
// been offered as a conflict. Rebuilt files carry the modified time the
// baseline recorded at the last sync, so a file untouched since then is
// recognised as in sync, and the server side is known unchanged because the
// folder's ETag still matches.
func TestRemoteTreePrunesUnchangedFoldersWithTheLiveBaseline(t *testing.T) {
	synced := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	f, e, local := adoptFixture(t, synced.UnixNano(), nil)

	remote, err := e.RemoteTree(context.Background(), local, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := f.pfCount("a"); n != 0 {
		t.Errorf("folder a was listed %d times, want 0: its ETag matches the baseline", n)
	}
	for _, rel := range []string{"a/f2", "a/b/f3"} {
		r, ok := remote[rel]
		if !ok {
			t.Fatalf("%s missing from a pruned folder", rel)
		}
		if !r.LastModified.Equal(synced) {
			t.Errorf("%s.LastModified = %v, want the baseline's %v: without it adopt calls every pruned file a conflict", rel, r.LastModified, synced)
		}
	}
	// End to end: a local file untouched since the last sync is in sync.
	p := filepath.Join(local, "a", "f2")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, synced, synced); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	if !engine.LocalMatchesRemote(fi, remote["a/f2"]) {
		t.Error("a file untouched since the last sync does not match its rebuilt entry")
	}
}

// A baseline that never recorded modified times cannot vouch for anything,
// so the scan crawls the server as before rather than replaying it.
func TestRemoteTreeCrawlsWhenTheBaselineHasNoModifiedTimes(t *testing.T) {
	f, e, local := adoptFixture(t, 0, nil)
	remote, err := e.RemoteTree(context.Background(), local, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := f.pfCount("a"); n != 1 {
		t.Errorf("folder a was listed %d times, want 1 (full crawl)", n)
	}
	if r := remote["a/f2"]; r.LastModified.IsZero() {
		t.Error("a crawled file has no modified time")
	}
}

// A selective-sync pair's baseline does not cover the folders left out, so
// it is not trusted either.
func TestRemoteTreeCrawlsForASelectiveSyncPair(t *testing.T) {
	f, e, local := adoptFixture(t, time.Now().UnixNano(), []string{"a/b"})
	if _, err := e.RemoteTree(context.Background(), local, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	if n := f.pfCount("a"); n != 1 {
		t.Errorf("folder a was listed %d times, want 1 (full crawl)", n)
	}
}

// Ignored paths stay out of the result when they come from the baseline too:
// the ignore filter only ever saw listed entries.
func TestRemoteTreeKeepsIgnoredPathsOutOfARebuiltFolder(t *testing.T) {
	_, e, local := adoptFixture(t, time.Now().UnixNano(), nil)
	skip := func(rel string) bool { return path.Base(rel) == "node_modules" }
	remote, err := e.RemoteTree(context.Background(), local, "", skip, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"a/node_modules", "a/node_modules/x"} {
		if _, ok := remote[rel]; ok {
			t.Errorf("%s came back from the baseline although it is ignored", rel)
		}
	}
	if _, ok := remote["a/f2"]; !ok {
		t.Error("a/f2 missing")
	}
}
