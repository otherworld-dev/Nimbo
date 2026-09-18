package agent

// Deck #557: a folder shared with you that is unshared (or a group folder you
// were removed from) vanishes from the WebDAV tree, which the diff reads as a
// deletion to mirror. These tests drive real passes against the fakeDAV to pin
// the whole chain — the scan records the share root, the baseline keeps it,
// the pass after the unshare keeps the local copy out of sync and tells the
// user, and the user's decision (keep / move / delete) is carried out.

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/otherworld/nimbo/internal/engine"
)

func TestBaselineForLocalCarriesMountRoot(t *testing.T) {
	r := engine.RemoteState{Path: "Team", ETag: "e", MountRoot: true}
	if b := baselineForLocal(t.TempDir(), "Team", r); !b.MountRoot {
		t.Errorf("clone-adopted row lost the flag: %+v", b)
	}
}

// sharedPair seeds a settled pair holding the user's own file plus a folder
// shared with them. Every node of the share carries S, as Nextcloud sends it.
func sharedPair(t *testing.T) (*fakeDAV, *Engine, Pair) {
	t.Helper()
	f := newFakeDAV(map[string]davNode{
		"":                 {isDir: true, etag: "e-root"},
		"mine.txt":         {etag: "e-mine", body: "mine"},
		"Team":             {isDir: true, etag: "e-team", perm: "SRGDNVCK"},
		"Team/Budget.xlsx": {etag: "e-budget", body: "numbers", perm: "SRGDNVW"},
		"Team/old":         {isDir: true, etag: "e-old", perm: "SRGDNVCK"},
		"Team/old/n.txt":   {etag: "e-n", body: "n", perm: "SRGDNVW"},
	})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	e, _ := newHookEngine(t, srv.URL)
	p := Pair{LocalDir: t.TempDir()}
	for i := 0; i < 2; i++ { // clone, then a quiet pass to settle it
		if _, err := e.SyncOnce(context.Background(), p); err != nil {
			t.Fatalf("seed sync %d: %v", i, err)
		}
	}
	return f, e, p
}

// unsharedPair is sharedPair after the owner stopped sharing "Team" and one
// pass has run: the copy is kept and parked.
func unsharedPair(t *testing.T) (*fakeDAV, *Engine, Pair) {
	t.Helper()
	f, e, p := sharedPair(t)
	for _, n := range []string{"Team/old/n.txt", "Team/old", "Team/Budget.xlsx", "Team"} {
		f.delNode(n)
	}
	f.setNode("", davNode{isDir: true, etag: "e-root2"})
	if _, err := e.SyncOnce(context.Background(), p); err != nil {
		t.Fatalf("pass after the unshare: %v", err)
	}
	return f, e, p
}

func localExists(t *testing.T, p Pair, rel string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(p.LocalDir, filepath.FromSlash(rel)))
	return err == nil
}

// writeMark is a snapshot of the fake server's write counters. Server writes
// are judged SINCE a mark rather than since the start: the seed passes have
// their own quirks (a just-downloaded file is occasionally re-uploaded by the
// settle pass), and those are not what these tests are about.
type writeMark struct {
	deletes int
	mkcol   int
	putN    map[string]int
}

func markWrites(f *fakeDAV) writeMark {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := writeMark{deletes: len(f.deletes), mkcol: f.mkcols["Team"], putN: map[string]int{}}
	for p, n := range f.putN {
		m.putN[p] = n
	}
	return m
}

// writesSince returns what the fake server has been asked to change since m.
func writesSince(f *fakeDAV, m writeMark) (deletes, puts []string, mkcolTeam int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	deletes = append([]string(nil), f.deletes[m.deletes:]...)
	mkcolTeam = f.mkcols["Team"] - m.mkcol
	for p, n := range f.putN {
		if n > m.putN[p] {
			puts = append(puts, p)
		}
	}
	sort.Strings(puts)
	return deletes, puts, mkcolTeam
}

// The whole share goes: keep the copy, forget its rows, say so, touch nothing
// on the server — and leave the copy OUT of sync until the user decides.
func TestUnshareKeepsTheLocalCopyOutOfSync(t *testing.T) {
	f, e, p := unsharedPair(t)
	st, _ := e.getStore()
	pk := PairKey(p.LocalDir, p.RemoteRoot)

	for _, rel := range []string{"Team/Budget.xlsx", "Team/old/n.txt"} {
		if !localExists(t, p, rel) {
			t.Errorf("local copy lost: %s", rel)
		}
	}
	b, _ := st.LoadBaseline(pk)
	for _, rel := range []string{"Team", "Team/Budget.xlsx", "Team/old", "Team/old/n.txt"} {
		if _, ok := b[rel]; ok {
			t.Errorf("baseline row kept for %s", rel)
		}
	}
	if _, ok := b["mine.txt"]; !ok {
		t.Error("an unrelated row was dropped")
	}
	told := false
	for _, ev := range e.recorder.Recent() {
		if ev.Kind == "unshared" && ev.Path == "Team" && ev.Local == p.LocalDir && ev.OK() {
			told = true
		}
	}
	if !told {
		t.Errorf("no 'unshared' activity event; got %+v", e.recorder.Recent())
	}
	list := e.DetachedFolders()
	if len(list) != 1 || list[0].Rel != "Team" || list[0].LocalDir != p.LocalDir ||
		list[0].LocalPath != filepath.Join(p.LocalDir, "Team") || list[0].Name != "Team" {
		t.Fatalf("detached list = %+v, want just Team", list)
	}

	// Parked: two more passes must neither upload the copy nor touch it.
	mark := markWrites(f)
	for i := 0; i < 2; i++ {
		if _, err := e.SyncOnce(context.Background(), p); err != nil {
			t.Fatalf("parked pass %d: %v", i, err)
		}
	}
	deletes, puts, mkcol := writesSince(f, mark)
	if len(deletes) != 0 || len(puts) != 0 || mkcol != 0 {
		t.Errorf("parked folder reached the server: DELETE %v, PUT %v, MKCOL Team ×%d", deletes, puts, mkcol)
	}
	if !localExists(t, p, "Team/Budget.xlsx") {
		t.Error("parked copy was removed")
	}
	if len(e.DetachedFolders()) != 1 {
		t.Error("parked entry did not survive the passes")
	}
}

// "Keep as my own": the copy becomes new local content and uploads to the
// user's account like any other folder.
func TestResolveDetachedKeep(t *testing.T) {
	f, e, p := unsharedPair(t)
	mark := markWrites(f)
	if err := e.ResolveDetached(filepath.Join(p.LocalDir, "Team"), "keep", ""); err != nil {
		t.Fatal(err)
	}
	if len(e.DetachedFolders()) != 0 {
		t.Fatal("entry not cleared")
	}
	if _, err := e.SyncOnce(context.Background(), p); err != nil {
		t.Fatalf("pass after keep: %v", err)
	}
	_, puts, mkcol := writesSince(f, mark)
	if mkcol == 0 {
		t.Error("kept folder was not recreated as the user's own")
	}
	if len(puts) != 2 || puts[0] != "Team/Budget.xlsx" || puts[1] != "Team/old/n.txt" {
		t.Errorf("kept files not uploaded as the user's own: %v", puts)
	}
}

// "Delete my copy": the local folder goes (to the Recycle Bin where there is
// one) and nothing reaches the server.
func TestResolveDetachedDelete(t *testing.T) {
	f, e, p := unsharedPair(t)
	mark := markWrites(f)
	if err := e.ResolveDetached(filepath.Join(p.LocalDir, "Team"), "delete", ""); err != nil {
		t.Fatal(err)
	}
	if localExists(t, p, "Team") {
		t.Fatal("local copy still present")
	}
	if len(e.DetachedFolders()) != 0 {
		t.Fatal("entry not cleared")
	}
	if _, err := e.SyncOnce(context.Background(), p); err != nil {
		t.Fatalf("pass after delete: %v", err)
	}
	deletes, puts, mkcol := writesSince(f, mark)
	if len(deletes) != 0 || len(puts) != 0 || mkcol != 0 {
		t.Errorf("server touched after a local delete: DELETE %v, PUT %v, MKCOL ×%d", deletes, puts, mkcol)
	}
}

// "Move it out of the sync folder": the folder lands under the chosen
// directory, leaves the sync folder, and nothing reaches the server.
func TestResolveDetachedMove(t *testing.T) {
	f, e, p := unsharedPair(t)
	mark := markWrites(f)
	dest := t.TempDir()
	if err := e.ResolveDetached(filepath.Join(p.LocalDir, "Team"), "move", dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "Team", "old", "n.txt")); err != nil {
		t.Errorf("moved copy incomplete: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "Team", "Budget.xlsx")); err != nil || string(b) != "numbers" {
		t.Errorf("moved file content: %q, %v", b, err)
	}
	if localExists(t, p, "Team") {
		t.Fatal("copy still in the sync folder")
	}
	if len(e.DetachedFolders()) != 0 {
		t.Fatal("entry not cleared")
	}
	if _, err := e.SyncOnce(context.Background(), p); err != nil {
		t.Fatalf("pass after move: %v", err)
	}
	deletes, puts, mkcol := writesSince(f, mark)
	if len(deletes) != 0 || len(puts) != 0 || mkcol != 0 {
		t.Errorf("server touched after a move: DELETE %v, PUT %v, MKCOL ×%d", deletes, puts, mkcol)
	}
}

// A move must not land back inside a sync folder (it would sync from there),
// must not overwrite an existing folder, and needs a real destination.
func TestResolveDetachedMoveRefusals(t *testing.T) {
	_, e, p := unsharedPair(t)
	cases := map[string]string{
		"inside the sync folder": filepath.Join(p.LocalDir, "elsewhere"),
		"the sync folder itself": p.LocalDir,
		"a missing directory":    filepath.Join(t.TempDir(), "nope"),
		"no destination":         "",
	}
	_ = os.MkdirAll(filepath.Join(p.LocalDir, "elsewhere"), 0o755)
	for name, dest := range cases {
		if err := e.ResolveDetached(filepath.Join(p.LocalDir, "Team"), "move", dest); err == nil {
			t.Errorf("%s: move accepted", name)
		}
	}
	taken := t.TempDir()
	_ = os.MkdirAll(filepath.Join(taken, "Team"), 0o755)
	if err := e.ResolveDetached(filepath.Join(p.LocalDir, "Team"), "move", taken); err == nil {
		t.Error("move onto an existing folder accepted")
	}
	// Every refusal left things as they were.
	if !localExists(t, p, "Team/Budget.xlsx") || len(e.DetachedFolders()) != 1 {
		t.Error("a refused move changed state")
	}
	if err := e.ResolveDetached(filepath.Join(p.LocalDir, "Team"), "sideways", ""); err == nil {
		t.Error("unknown choice accepted")
	}
	if err := e.ResolveDetached(filepath.Join(p.LocalDir, "Nope"), "keep", ""); err == nil {
		t.Error("unknown folder accepted")
	}
}

// A subfolder deleted INSIDE the share by its owner is an ordinary deletion and
// must still be mirrored.
func TestDeletionInsideAShareStillMirrors(t *testing.T) {
	f, e, p := sharedPair(t)
	f.delNode("Team/old/n.txt")
	f.delNode("Team/old")
	f.setNode("Team", davNode{isDir: true, etag: "e-team2", perm: "SRGDNVCK"})
	f.setNode("", davNode{isDir: true, etag: "e-root2"})

	if _, err := e.SyncOnce(context.Background(), p); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if localExists(t, p, "Team/old") {
		t.Error("deleted subfolder of the share was kept")
	}
	if !localExists(t, p, "Team/Budget.xlsx") {
		t.Error("untouched file inside the share was lost")
	}
	for _, ev := range e.recorder.Recent() {
		if ev.Kind == "unshared" {
			t.Errorf("a deletion inside a share reported as an unshare: %+v", ev)
		}
	}
	if len(e.DetachedFolders()) != 0 {
		t.Error("a deletion inside a share was parked")
	}
}

// --- On-demand mode: the copy is moved OUT of the cloud folder --------------
//
// A cloud sync root can only show truthful state for things the server has,
// so an unshared folder's salvaged copy cannot stay inside it. The VFS watcher
// salvages the bytes in place and then hands the folder to ParkDetachedCopy,
// which moves it next to the sync folder and parks it. The three choices then
// work on that location, with "keep" moving it back in.

// parkedEngine is a bare engine whose "sync folder" is a temp dir holding a
// Team folder with two files — what the VFS watcher would hand over.
func parkedEngine(t *testing.T) (*Engine, string, string) {
	t.Helper()
	e, _ := newHookEngine(t, "http://127.0.0.1:9") // never dialled
	base := filepath.Join(t.TempDir(), "Nimbo")
	team := filepath.Join(base, "Team")
	if err := os.MkdirAll(filepath.Join(team, "old"), 0o755); err != nil {
		t.Fatal(err)
	}
	for rel, body := range map[string]string{"Budget.xlsx": "numbers", "old/n.txt": "n"} {
		if err := os.WriteFile(filepath.Join(team, filepath.FromSlash(rel)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return e, base, team
}

func TestParkDetachedCopyMovesItBesideTheSyncFolder(t *testing.T) {
	e, base, team := parkedEngine(t)
	var toastMsg string
	e.onToast = func(title, msg, link string) { toastMsg = title + "|" + msg + "|" + link }

	parkedAt, err := e.ParkDetachedCopy(base, "", "Team", team)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(filepath.Dir(base), "Nimbo - no longer shared", "Team")
	if parkedAt != want {
		t.Errorf("parkedAt = %q, want %q", parkedAt, want)
	}
	if b, err := os.ReadFile(filepath.Join(parkedAt, "old", "n.txt")); err != nil || string(b) != "n" {
		t.Errorf("moved copy incomplete: %q %v", b, err)
	}
	if _, err := os.Stat(team); err == nil {
		t.Error("copy still inside the sync folder")
	}
	list := e.DetachedFolders()
	if len(list) != 1 || list[0].Rel != "Team" || list[0].LocalPath != parkedAt || !list[0].MovedOut {
		t.Fatalf("detached list = %+v", list)
	}
	if !strings.Contains(toastMsg, "Team") || !strings.Contains(toastMsg, "no longer shared") || !strings.Contains(toastMsg, "action=detached") {
		t.Errorf("toast = %q", toastMsg)
	}
}

// A second unshare of a folder with the same name must not overwrite the copy
// already parked under it.
func TestParkDetachedCopyNeverOverwrites(t *testing.T) {
	e, base, team := parkedEngine(t)
	first, err := e.ParkDetachedCopy(base, "", "Team", team)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(team, 0o755); err != nil { // shared again, then unshared again
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(team, "v2.txt"), []byte("2"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := e.ParkDetachedCopy(base, "", "Team", team)
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatalf("second park reused %q", first)
	}
	if _, err := os.Stat(filepath.Join(first, "Budget.xlsx")); err != nil {
		t.Error("first parked copy was clobbered")
	}
	if _, err := os.Stat(filepath.Join(second, "v2.txt")); err != nil {
		t.Error("second parked copy missing")
	}
	if len(e.DetachedFolders()) != 2 {
		t.Errorf("both should be listed: %+v", e.DetachedFolders())
	}
}

func TestResolveParkedKeepMovesItBackIn(t *testing.T) {
	e, base, team := parkedEngine(t)
	parkedAt, err := e.ParkDetachedCopy(base, "", "Team", team)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ResolveDetached(parkedAt, "keep", ""); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(team, "Budget.xlsx")); err != nil || string(b) != "numbers" {
		t.Errorf("copy not back in the sync folder: %q %v", b, err)
	}
	if _, err := os.Stat(parkedAt); err == nil {
		t.Error("parked copy still at the parking spot")
	}
	if len(e.DetachedFolders()) != 0 {
		t.Error("entry not cleared")
	}
}

// If the folder exists in the sync folder again (shared again, or the user made
// one), "keep" must not merge or overwrite — refuse and leave everything as is.
func TestResolveParkedKeepRefusesWhenThePathIsTaken(t *testing.T) {
	e, base, team := parkedEngine(t)
	parkedAt, err := e.ParkDetachedCopy(base, "", "Team", team)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(team, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := e.ResolveDetached(parkedAt, "keep", ""); err == nil {
		t.Fatal("keep accepted over an existing folder")
	}
	if len(e.DetachedFolders()) != 1 {
		t.Error("entry lost after a refused keep")
	}
}

func TestResolveParkedMoveAndDeleteActOnTheParkedCopy(t *testing.T) {
	e, base, team := parkedEngine(t)
	parkedAt, err := e.ParkDetachedCopy(base, "", "Team", team)
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err := e.ResolveDetached(parkedAt, "move", dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "Team", "Budget.xlsx")); err != nil {
		t.Error("move did not take the parked copy")
	}
	if _, err := os.Stat(parkedAt); err == nil {
		t.Error("parked copy still present after move")
	}

	e2, base2, team2 := parkedEngine(t)
	parkedAt2, err := e2.ParkDetachedCopy(base2, "", "Team", team2)
	if err != nil {
		t.Fatal(err)
	}
	if err := e2.ResolveDetached(parkedAt2, "delete", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(parkedAt2); err == nil {
		t.Error("parked copy still present after delete")
	}
	if len(e2.DetachedFolders()) != 0 {
		t.Error("entry not cleared after delete")
	}
}

// A parking spot must never sit inside a synced folder: a sync folder at a
// drive root would otherwise get its own sibling INSIDE itself. Fall back to the
// user's home directory.
func TestParkingDirFallsBackOutsideSyncFolders(t *testing.T) {
	e, _ := newHookEngine(t, "http://127.0.0.1:9")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	got := e.parkingDir(filepath.VolumeName(home) + string(filepath.Separator))
	if !strings.HasPrefix(got, home) {
		t.Errorf("parkingDir(drive root) = %q, want it under %q", got, home)
	}
}
