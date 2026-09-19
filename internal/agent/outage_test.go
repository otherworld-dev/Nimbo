package agent

// A server outage must never read as a deletion (Deck #691).
//
// 2026-09-19, live mode: the server was unreachable for 13 minutes. A local
// change inside "To Sort" put the folder itself into a scoped SyncPaths batch;
// the folder's Stat failed, the failure was taken for "not on the server", and
// the whole folder (180,810 files) was deleted locally. These tests pin each
// link of that chain.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/otherworld/nimbo/internal/engine"
	"github.com/otherworld/nimbo/internal/state"
)

// seededFolderPair builds a settled pair holding one folder of n files, so the
// damage guard is armed exactly as on a long-running install. A file beside the
// folder keeps the root non-empty: an empty root trips a different guard.
func seededFolderPair(t *testing.T, dir string, n int) (*fakeDAV, *Engine, Pair) {
	t.Helper()
	nodes := map[string]davNode{
		"":         {isDir: true, etag: "e-root"},
		dir:        {isDir: true, etag: "e-dir"},
		"keep.txt": {etag: "e-keep", body: "k"},
	}
	for i := 0; i < n; i++ {
		nodes[fmt.Sprintf("%s/f%03d.txt", dir, i)] = davNode{etag: fmt.Sprintf("e%03d", i), body: "v1"}
	}
	f := newFakeDAV(nodes)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	e, _ := newHookEngine(t, srv.URL)

	p := Pair{LocalDir: t.TempDir()}
	for _, pass := range []string{"seed", "settle"} {
		if _, err := e.SyncOnce(context.Background(), p); err != nil {
			t.Fatalf("%s sync: %v", pass, err)
		}
	}
	return f, e, p
}

func assertLocalFiles(t *testing.T, p Pair, dir string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		name := filepath.Join(p.LocalDir, dir, fmt.Sprintf("f%03d.txt", i))
		if _, err := os.Stat(name); err != nil {
			t.Fatalf("local file %s is gone: %v", name, err)
		}
	}
}

// The incident itself: the folder's Stat fails, and the folder must survive.
func TestSyncPathsKeepsAFolderTheServerCouldNotBeAsked(t *testing.T) {
	f, e, p := seededFolderPair(t, "To Sort", 3)
	f.setFailPF("To Sort", http.StatusBadGateway)

	// A file created in the folder: the watcher reports the folder too.
	if err := os.WriteFile(filepath.Join(p.LocalDir, "To Sort", "new.txt"), []byte("n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := e.SyncPaths(context.Background(), p, []string{"To Sort", "To Sort/new.txt"})
	if err == nil {
		t.Fatal("a pass that could not ask the server about a path must fail, not guess")
	}
	assertLocalFiles(t, p, "To Sort", 3)
	if d := f.deletePaths(); len(d) != 0 {
		t.Fatalf("an unreachable server must not cause server deletions, got %v", d)
	}
}

// A 404 is only an answer if Nextcloud gave it. A reverse proxy in front of a
// stopped server can 404 every path — the pair's own root included, which a
// real server never reports missing.
func TestSyncPathsKeepsAFolderWhenEverythingIs404(t *testing.T) {
	f, e, p := seededFolderPair(t, "To Sort", 3)
	f.setFailPF("", http.StatusNotFound)
	f.setFailPF("To Sort", http.StatusNotFound)

	if _, err := e.SyncPaths(context.Background(), p, []string{"To Sort"}); err == nil {
		t.Fatal("a 404 the server's own root shares is not a deletion; the pass must fail")
	}
	assertLocalFiles(t, p, "To Sort", 3)
}

// The checks above must not overcorrect: a folder the server really deleted is
// still deleted here.
func TestSyncPathsStillMirrorsARealServerDeletion(t *testing.T) {
	f, e, p := seededFolderPair(t, "Small", 3)
	for i := 0; i < 3; i++ {
		f.delNode(fmt.Sprintf("Small/f%03d.txt", i))
	}
	f.delNode("Small")

	if _, err := e.SyncPaths(context.Background(), p, []string{"Small"}); err != nil {
		t.Fatalf("SyncPaths: %v", err)
	}
	if _, err := os.Stat(filepath.Join(p.LocalDir, "Small")); !os.IsNotExist(err) {
		t.Fatalf("a folder the server deleted is still here locally (err=%v)", err)
	}
}

// Only a TRACKED path, or a transient failure, must fail the pass. A refusal
// (4xx) of a new file (say a name the server forbids) would otherwise fail
// every batch that touches it, holding up the rest of the batch.
func TestSyncPathsToleratesARefusedUntrackedPath(t *testing.T) {
	f, e, p := seededFolderPair(t, "To Sort", 1)
	for _, name := range []string{"refused.txt", "ok.txt"} {
		if err := os.WriteFile(filepath.Join(p.LocalDir, "To Sort", name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.setFailPF("To Sort/refused.txt", http.StatusBadRequest)

	if _, err := e.SyncPaths(context.Background(), p, []string{"To Sort/refused.txt", "To Sort/ok.txt"}); err != nil {
		t.Fatalf("a refused untracked path failed the whole pass: %v", err)
	}
	if f.putBody("To Sort/ok.txt") != "ok.txt" {
		t.Fatalf("the rest of the batch was not synced: %v", f.putPaths())
	}
}

// An ignored path is not synced, so it is not asked about either: its Stat
// failing must not fail the pass.
func TestSyncPathsNeverAsksAboutAnIgnoredPath(t *testing.T) {
	f, e, p := seededFolderPair(t, "To Sort", 1)
	if err := os.WriteFile(filepath.Join(p.LocalDir, "To Sort", "x.tmp"), []byte("t"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.setFailPF("To Sort/x.tmp", http.StatusBadGateway)

	if _, err := e.SyncPaths(context.Background(), p, []string{"To Sort/x.tmp"}); err != nil {
		t.Fatalf("an ignored path failed the pass: %v", err)
	}
	if n := f.pfCount("To Sort/x.tmp"); n != 0 {
		t.Fatalf("an ignored path was stat'd %d time(s)", n)
	}
}

// The damage guard must weigh a folder delete by what it holds. The server
// really deleting most of what this pair knows is the guard's whole job, but a
// folder arriving as ONE action counted as one deletion and sailed under it.
func TestGuardWeighsAFolderDeletedOnTheServer(t *testing.T) {
	f, e, p := seededFolderPair(t, "Big", 60)
	for i := 0; i < 60; i++ {
		f.delNode(fmt.Sprintf("Big/f%03d.txt", i))
	}
	f.delNode("Big")

	_, err := e.SyncPaths(context.Background(), p, []string{"Big"})
	if err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("deleting a folder holding most of the pair must pause it, got err=%v", err)
	}
	assertLocalFiles(t, p, "Big", 60)
}

// The server-side bulk-delete guard, same flaw: a folder removed locally went
// to the server as one DELETE, however many files it held.
func TestBulkDeleteGuardWeighsAFolderDeletedLocally(t *testing.T) {
	f, e, p := seededFolderPair(t, "Big", 60)
	if err := os.RemoveAll(filepath.Join(p.LocalDir, "Big")); err != nil {
		t.Fatal(err)
	}

	_, err := e.SyncPaths(context.Background(), p, []string{"Big"})
	if err == nil {
		t.Fatal("deleting a folder holding most of the pair on the server must be refused")
	}
	if d := f.deletePaths(); len(d) != 0 {
		t.Fatalf("the refused pass still deleted on the server: %v", d)
	}
}

// Nimbo's own partial download is not a user file. A scoped pass tried to
// upload it, and every write to it put its folder back into a batch.
func TestSyncPathsNeverUploadsAPartialDownload(t *testing.T) {
	f, e, p := seededFolderPair(t, "To Sort", 1)
	part := filepath.Join(p.LocalDir, "To Sort", "OnlineArchive.pst.nimbo-part")
	if err := os.WriteFile(part, []byte("half a download"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.SyncPaths(context.Background(), p, []string{"To Sort/OnlineArchive.pst.nimbo-part"}); err != nil {
		t.Fatalf("SyncPaths: %v", err)
	}
	// Asserted by name, so the test says what it is about: no partial file
	// reaches the server, whatever else a pass uploads.
	for _, got := range f.putPaths() {
		if strings.HasSuffix(got, ".nimbo-part") {
			t.Fatalf("a partial download was uploaded: %s", got)
		}
	}
}

// deletionWeight is what the guards now count. A folder weighs what it holds;
// a full scan, which already has one action per file, must weigh the same as
// before (no double count); and "Big b" is a sibling of "Big", not inside it.
func TestDeletionWeight(t *testing.T) {
	st := maintTestStore(t)
	const pk = "P"
	for _, p := range []string{"Big", "Big/a", "Big/sub", "Big/sub/b", "Big b", "Big b/c", "file.txt"} {
		if err := st.UpsertBaseline(pk, engine.BaselineState{Path: p}); err != nil {
			t.Fatal(err)
		}
	}
	del := func(k engine.ActionKind, paths ...string) []engine.Action {
		var out []engine.Action
		for _, p := range paths {
			out = append(out, engine.Action{Kind: k, Path: p})
		}
		return out
	}
	for _, tc := range []struct {
		name    string
		actions []engine.Action
		want    int
	}{
		{"nothing", nil, 0},
		{"one folder action", del(engine.ActDeleteLocal, "Big"), 4},
		{"full-scan shape", del(engine.ActDeleteLocal, "Big", "Big/a", "Big/sub", "Big/sub/b"), 4},
		{"siblings", del(engine.ActDeleteLocal, "Big", "Big b/c", "file.txt"), 6},
		{"other kinds ignored", del(engine.ActDeleteRemote, "Big"), 0},
		// A folder rename: rename coalescing pairs the FILES into moves and
		// leaves the old folder's delete, so the moved rows are not deleted.
		{"folder rename", append(del(engine.ActDeleteLocal, "Big"),
			engine.Action{Kind: engine.ActMoveLocal, Path: "Big/a", Dest: "New/a"},
			engine.Action{Kind: engine.ActMoveLocal, Path: "Big/sub/b", Dest: "New/sub/b"}), 2},
		{"moves the other way do not count", append(del(engine.ActDeleteLocal, "Big"),
			engine.Action{Kind: engine.ActMoveRemote, Path: "Big/a", Dest: "New/a"}), 4},
	} {
		got, err := deletionWeight(st, pk, tc.actions, engine.ActDeleteLocal)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: weight %d, want %d", tc.name, got, tc.want)
		}
	}
}

// A server copy that fails its checksum (a torn upload from another computer)
// was downloaded again on every pass: 24 GB every 20 minutes for days. It is
// fetched once, then skipped until the server's copy changes, and fetched as
// soon as it does.
func TestADamagedServerCopyIsNotDownloadedEveryPass(t *testing.T) {
	f, e, p := seededFolderPair(t, "To Sort", 1)
	const pst = "To Sort/archive.pst"
	f.setNode(pst, davNode{etag: "torn", body: "torn bytes", checksum: "4f663abde826ad82d8ff238365e1f9f3e2dd81af"})
	f.setNode("To Sort", davNode{isDir: true, etag: "e-dir2"})
	f.setNode("", davNode{isDir: true, etag: "e-root2"})

	for pass := 0; pass < 3; pass++ {
		_, _ = e.SyncOnce(context.Background(), p)
	}
	if n := f.getCount(pst); n != 1 {
		t.Fatalf("the damaged copy was downloaded %d times over 3 passes, want 1", n)
	}
	if _, err := os.Stat(filepath.Join(p.LocalDir, filepath.FromSlash(pst))); !os.IsNotExist(err) {
		t.Fatalf("a damaged copy landed locally (err=%v)", err)
	}

	// Uploaded again, cleanly: it must come down on the next pass.
	f.setNode(pst, davNode{etag: "clean", body: "good bytes"})
	f.setNode("To Sort", davNode{isDir: true, etag: "e-dir3"})
	f.setNode("", davNode{isDir: true, etag: "e-root3"})
	if _, err := e.SyncOnce(context.Background(), p); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(p.LocalDir, filepath.FromSlash(pst))); err != nil || string(b) != "good bytes" {
		t.Fatalf("the clean copy was not downloaded: %q err=%v", b, err)
	}
}

// shareRootPair seeds a pair holding a folder shared with the user ("Team") and a
// file shared on its own ("Budget.xlsx"), so both are share roots.
func shareRootPair(t *testing.T) (*fakeDAV, *Engine, *state.Store, Pair) {
	t.Helper()
	f := newFakeDAV(map[string]davNode{
		"":            {isDir: true, etag: "e-root"},
		"Team":        {isDir: true, etag: "e-team", perm: "SRGDNVCK"},
		"Team/r.txt":  {etag: "r1", body: "v1", perm: "SRGDNVW"},
		"Budget.xlsx": {etag: "b1", body: "b1", perm: "SRGDNVW"},
		"keep.txt":    {etag: "k", body: "k"},
	})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	e, st := newHookEngine(t, srv.URL)
	p := Pair{LocalDir: t.TempDir()}
	for i := 0; i < 2; i++ {
		if _, err := e.SyncOnce(context.Background(), p); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	rows, _ := st.LoadBaseline(PairKey(p.LocalDir, p.RemoteRoot))
	if !rows["Team"].MountRoot || !rows["Budget.xlsx"].MountRoot {
		t.Fatalf("precondition: share roots not flagged: Team=%+v Budget=%+v", rows["Team"], rows["Budget.xlsx"])
	}
	return f, e, st, p
}

// A quick sync builds the server's state from a bare Stat, which can't tell a
// share's root from anything inside it. Writing rows from that map dropped the
// root flag, and an unshare then recycled the copy instead of keeping it (#557).
func TestSyncPathsKeepsTheShareRootFlagOfAnEditedSharedFile(t *testing.T) {
	_, e, st, p := shareRootPair(t)
	if err := os.WriteFile(filepath.Join(p.LocalDir, "Budget.xlsx"), []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.SyncPaths(context.Background(), p, []string{"Budget.xlsx"}); err != nil {
		t.Fatalf("SyncPaths: %v", err)
	}
	rows, _ := st.LoadBaseline(PairKey(p.LocalDir, p.RemoteRoot))
	if !rows["Budget.xlsx"].MountRoot {
		t.Fatalf("an upload through SyncPaths cleared the share root flag: %+v", rows["Budget.xlsx"])
	}
}

func TestSyncPathsKeepsTheShareRootFlagOfADirtiedFolder(t *testing.T) {
	f, e, st, p := shareRootPair(t)
	f.setNode("Team/r.txt", davNode{etag: "r2", body: "v2", perm: "SRGDNVW"})
	f.setFailGET("Team/r.txt", http.StatusForbidden) // the download fails, so "Team" is dirtied
	_, _ = e.SyncPaths(context.Background(), p, []string{"Team", "Team/r.txt"})

	rows, _ := st.LoadBaseline(PairKey(p.LocalDir, p.RemoteRoot))
	if !rows["Team"].MountRoot {
		t.Fatalf("dirtying through SyncPaths cleared the share root flag: %+v", rows["Team"])
	}
}

// A pass cancelled partway (quit, pause, a restart) left its remaining
// transfers undone but still stamped their folders as seen at the server's
// current version, so the next scan skipped those folders and the files it
// never fetched stayed invisible until something else there changed. Folder A's
// transfers are in flight when it stops, and fail, which keeps A rescanned; B's
// never start, and nothing marks B unfinished.
func TestACancelledPassLeavesItsUnfinishedFoldersToBeScannedAgain(t *testing.T) {
	nodes := map[string]davNode{
		"":         {isDir: true, etag: "e-root"},
		"A":        {isDir: true, etag: "e-a"},
		"B":        {isDir: true, etag: "e-b"},
		"keep.txt": {etag: "k", body: "k"},
	}
	f := newFakeDAV(nodes)
	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.Contains(r.URL.Path, "/A/new") {
			once.Do(cancel) // "quit" as soon as the first new file starts
		}
		f.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	e, _ := newHookEngine(t, srv.URL)
	p := Pair{LocalDir: t.TempDir()}
	for i := 0; i < 2; i++ {
		if _, err := e.SyncOnce(context.Background(), p); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	const n = 8
	for _, dir := range []string{"A", "B"} {
		for i := 0; i < n; i++ {
			f.setNode(fmt.Sprintf("%s/new%02d.txt", dir, i), davNode{etag: fmt.Sprintf("%s%02d", dir, i), body: "new"})
		}
		f.setNode(dir, davNode{isDir: true, etag: "e-" + dir + "2"})
	}
	f.setNode("", davNode{isDir: true, etag: "e-root2"})
	_, _ = e.SyncOnce(ctx, p) // cancelled partway

	if _, err := e.SyncOnce(context.Background(), p); err != nil {
		t.Fatalf("the pass after: %v", err)
	}
	for _, dir := range []string{"A", "B"} {
		for i := 0; i < n; i++ {
			name := filepath.Join(p.LocalDir, dir, fmt.Sprintf("new%02d.txt", i))
			if _, err := os.Stat(name); err != nil {
				t.Fatalf("a file the cancelled pass never fetched stayed invisible: %v", err)
			}
		}
	}
}

// A file changed on both sides is a conflict, not a download, and conflict
// handling fetched the server's copy on its own, past the damaged-copy skip: a
// damaged copy of a file also edited here (a .pst open in Outlook on two
// computers) was fetched in full on every pass.
func TestADamagedServerCopyInAConflictIsNotFetchedEveryPass(t *testing.T) {
	f, e, p := seededFolderPair(t, "To Sort", 1)
	const doc = "To Sort/f000.txt"
	local := filepath.Join(p.LocalDir, filepath.FromSlash(doc))
	if err := os.WriteFile(local, []byte("local edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.setNode(doc, davNode{etag: "torn", body: "torn bytes", checksum: "4f663abde826ad82d8ff238365e1f9f3e2dd81af"})
	f.setNode("To Sort", davNode{isDir: true, etag: "e-dir2"})
	f.setNode("", davNode{isDir: true, etag: "e-root2"})
	before := f.getCount(doc)

	for pass := 0; pass < 3; pass++ {
		_, _ = e.SyncOnce(context.Background(), p)
	}
	if n := f.getCount(doc) - before; n != 1 {
		t.Fatalf("the damaged copy was fetched %d times over 3 passes, want 1", n)
	}
	// Holding the conflict back also held the local edit back, silently and for
	// as long as the damaged copy stayed. It is kept both ways instead, without
	// the server's bytes: the edit goes up as the conflicted copy, and the
	// original name waits for a good server copy. Nothing is deleted.
	var sent string
	for _, put := range f.putPaths() {
		if strings.HasPrefix(put, "To Sort/f000 (conflicted copy") {
			sent = f.putBody(put)
		}
	}
	if sent != "local edit" {
		t.Fatalf("the local edit never reached the server (uploads: %v)", f.putPaths())
	}
	if d := f.deletePaths(); len(d) != 0 {
		t.Fatalf("keeping both deleted on the server: %v", d)
	}
}

// The first sync (the clone) didn't record a damaged copy either, so a new
// computer fetched it again on the very next pass.
func TestTheFirstSyncRecordsADamagedCopy(t *testing.T) {
	f := newFakeDAV(map[string]davNode{
		"":            {isDir: true, etag: "e-root"},
		"archive.pst": {etag: "torn", body: "torn bytes", checksum: "4f663abde826ad82d8ff238365e1f9f3e2dd81af"},
		"ok.txt":      {etag: "ok", body: "ok"},
	})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	e, _ := newHookEngine(t, srv.URL)
	p := Pair{LocalDir: t.TempDir()}
	for pass := 0; pass < 3; pass++ {
		_, _ = e.SyncOnce(context.Background(), p)
	}
	if n := f.getCount("archive.pst"); n != 1 {
		t.Fatalf("the damaged copy was fetched %d times from the first sync on, want 1", n)
	}
}

// A quick sync that can't reach the server says so, the way a full pass does.
// It returned the error with the status line left on whatever it said before,
// "Up to date" included.
func TestAQuickSyncThatCannotReachTheServerSaysOffline(t *testing.T) {
	f, e, p := seededFolderPair(t, "To Sort", 1)
	e.status("Up to date")
	f.setFailPF("To Sort", http.StatusBadGateway)
	if _, err := e.SyncPaths(context.Background(), p, []string{"To Sort"}); err == nil {
		t.Fatal("expected the pass to fail")
	}
	e.diagMu.Lock()
	got := e.lastStatus
	e.diagMu.Unlock()
	if got != "Offline" {
		t.Fatalf("status = %q, want Offline", got)
	}
}
