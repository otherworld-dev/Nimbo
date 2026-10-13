package agent

// Explorer's "Sync this first" (Deck #702, stage 3): which of the lane, a
// quick pass of named paths, or nothing at all a path gets.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/otherworld/nimbo/internal/config"
	"github.com/otherworld/nimbo/internal/transport"
)

// standIn is what a pair's watcher listens on, so a test can see what
// SyncFirst asked it for without running one.
type standIn struct {
	nudge      chan string
	user, full chan struct{}
	done       chan struct{}
}

func watchStandIn(t *testing.T, e *Engine, p Pair) *standIn {
	t.Helper()
	w := &standIn{nudge: make(chan string, 1024), user: make(chan struct{}, 1), full: make(chan struct{}, 1), done: make(chan struct{})}
	pk := PairKey(p.LocalDir, p.RemoteRoot)
	e.watchMu.Lock()
	e.nudges = map[string]chan string{pk: w.nudge}
	e.triggersUser = map[string]chan struct{}{pk: w.user}
	e.triggersFull = map[string]chan struct{}{pk: w.full}
	e.watchDone = map[string]chan struct{}{pk: w.done}
	e.watchMu.Unlock()
	t.Cleanup(func() { close(w.done) })
	return w
}

// nudged collects the n paths the watcher is handed, relative to the pair.
func (w *standIn) nudged(t *testing.T, p Pair, n int) []string {
	t.Helper()
	var got []string
	for len(got) < n {
		select {
		case abs := <-w.nudge:
			rel, err := filepath.Rel(p.LocalDir, abs)
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, filepath.ToSlash(rel))
		case <-time.After(5 * time.Second):
			t.Fatalf("the watcher was handed %v, want %d paths", got, n)
		}
	}
	sort.Strings(got)
	return got
}

func (w *standIn) nothingNudged(t *testing.T) {
	t.Helper()
	select {
	case abs := <-w.nudge:
		t.Fatalf("the watcher was handed %s", abs)
	case <-time.After(100 * time.Millisecond):
	}
}

func fired(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// syncFirstPair is a settled live pair (a folder D beside keep.txt) on record
// as one of the account's sync folders, with a stand-in for its watcher.
func syncFirstPair(t *testing.T) (*Engine, Pair, *standIn) {
	t.Helper()
	_, _, e, p := lanePair(t, "PUT", "never")
	if err := e.dirs.SavePairs([]config.SyncPair{{LocalDir: p.LocalDir}}); err != nil {
		t.Fatal(err)
	}
	return e, p, watchStandIn(t, e, p)
}

// queueJob puts a waiting large transfer for rel into the engine's lane. The
// file is on disk, as a waiting upload's is.
func queueJob(t *testing.T, e *Engine, p Pair, rel string) *testJob {
	t.Helper()
	writeLocal(t, p.LocalDir, rel, "x")
	tj := newTestJob(PairKey(p.LocalDir, p.RemoteRoot), rel)
	tj.j.dir = p.LocalDir
	tj.j.abs = filepath.Join(p.LocalDir, filepath.FromSlash(rel))
	if !e.transferLane().add(tj.j) {
		t.Fatalf("lane refused %s", rel)
	}
	return tj
}

func waitingOrder(e *Engine) []string {
	var out []string
	for _, en := range e.LaneEntries() {
		if en.State == "waiting" || en.State == "paused" {
			out = append(out, en.Path)
		}
	}
	return out
}

func abs(p Pair, rel string) string { return filepath.Join(p.LocalDir, filepath.FromSlash(rel)) }

func TestSyncFirstMovesAWaitingLargeFileToTheFront(t *testing.T) {
	e, p, w := syncFirstPair(t)
	a, b := queueJob(t, e, p, "D/a.bin"), queueJob(t, e, p, "D/b.bin")
	waitFor(t, "the two workers to be busy", func() bool { return a.starts.Load() == 1 && b.starts.Load() == 1 })
	queueJob(t, e, p, "D/c.bin")
	queueJob(t, e, p, "D/d.bin")

	res := e.SyncFirst(abs(p, "D/d.bin"))
	if res.Kind != SyncFirstAsked || res.Lane != 1 || res.Running || res.Paths != 0 {
		t.Fatalf("got %+v, want one large transfer moved", res)
	}
	if got := fmt.Sprint(waitingOrder(e)); got != "[D/d.bin D/c.bin]" {
		t.Fatalf("queue is %s, want d ahead of c", got)
	}
	// The lane has the file, so a pass would only skip it: no quick pass.
	w.nothingNudged(t)
	if a.starts.Load() != 1 || b.starts.Load() != 1 {
		t.Fatal("a running transfer was interrupted")
	}
}

func TestSyncFirstLeavesARunningLargeFileAlone(t *testing.T) {
	e, p, w := syncFirstPair(t)
	a := queueJob(t, e, p, "D/a.bin")
	waitFor(t, "a to start", func() bool { return a.starts.Load() == 1 })

	res := e.SyncFirst(abs(p, "D/a.bin"))
	if res.Kind != SyncFirstAsked || !res.Running || res.Lane != 0 || res.Paths != 0 {
		t.Fatalf("got %+v, want only Running", res)
	}
	w.nothingNudged(t)
	noDone(t, a, "a running transfer asked to sync first")
}

func TestSyncFirstBringsBackAFileThatWasSetAside(t *testing.T) {
	e, p, _ := syncFirstPair(t)
	e.transferLane().pause() // nothing starts, so the job is set aside while waiting
	a := queueJob(t, e, p, "D/a.bin")
	if !e.LaneSetAside(abs(p, "D/a.bin"), time.Time{}) {
		t.Fatal("could not set the file aside")
	}

	res := e.SyncFirst(abs(p, "D/a.bin"))
	if res.Kind != SyncFirstAsked || res.Lane != 1 {
		t.Fatalf("got %+v, want the set-aside file brought back", res)
	}
	select {
	case err := <-a.doneErr:
		if err != errLaneUnparked {
			t.Fatalf("the set-aside job left with %v, want errLaneUnparked", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the set-aside job never left the lane")
	}
	// The pass that plans it again hands it back to the front, not the back.
	queueJob(t, e, p, "D/other.bin")
	queueJob(t, e, p, "D/a.bin")
	if got := fmt.Sprint(waitingOrder(e)); got != "[D/a.bin D/other.bin]" {
		t.Fatalf("queue is %s, want the file that was asked for first", got)
	}
}

func TestSyncFirstNudgesAFileTheLaneDoesNotHold(t *testing.T) {
	e, p, w := syncFirstPair(t)
	writeLocal(t, p.LocalDir, "D/note.txt", "edited")

	res := e.SyncFirst(abs(p, "D/note.txt"))
	if res.Kind != SyncFirstAsked || res.Paths != 1 || res.Lane != 0 {
		t.Fatalf("got %+v, want one path handed to a quick pass", res)
	}
	if got := fmt.Sprint(w.nudged(t, p, 1)); got != "[D/note.txt]" {
		t.Fatalf("nudged %s", got)
	}
	if fired(w.user) || fired(w.full) {
		t.Fatal("a single file asked for a pass of the whole sync folder")
	}
}

// A large file the lane does not hold yet is planned by the quick pass and
// handed over then. It must join the front, not wait behind the others.
func TestSyncFirstPutsALargeFileNotYetInTheLaneAtTheFront(t *testing.T) {
	e, p, _ := syncFirstPair(t)
	e.transferLane().pause()
	queueJob(t, e, p, "D/a.bin")
	queueJob(t, e, p, "D/b.bin")
	writeLocal(t, p.LocalDir, "D/urgent.bin", "x")

	if res := e.SyncFirst(abs(p, "D/urgent.bin")); res.Kind != SyncFirstAsked || res.Paths != 1 {
		t.Fatalf("got %+v", res)
	}
	queueJob(t, e, p, "D/urgent.bin") // the quick pass handing it over
	queueJob(t, e, p, "D/z.bin")      // an unrelated one still joins the back
	if got := fmt.Sprint(waitingOrder(e)); got != "[D/urgent.bin D/a.bin D/b.bin D/z.bin]" {
		t.Fatalf("queue is %s", got)
	}
}

func TestSyncFirstDoesNothingWhilePaused(t *testing.T) {
	e, p, w := syncFirstPair(t)
	e.SetPaused(true)
	queueJob(t, e, p, "D/a.bin")
	queueJob(t, e, p, "D/b.bin")

	for _, rel := range []string{"D/b.bin", "keep.txt", "D"} {
		if res := e.SyncFirst(abs(p, rel)); res.Kind != SyncFirstPaused {
			t.Fatalf("%s: got %+v, want SyncFirstPaused", rel, res)
		}
	}
	if got := fmt.Sprint(waitingOrder(e)); got != "[D/a.bin D/b.bin]" {
		t.Fatalf("the queue was reordered while paused: %s", got)
	}
	w.nothingNudged(t)
	if fired(w.user) || fired(w.full) {
		t.Fatal("a sync was asked for while paused")
	}
}

func TestSyncFirstOutsideEverySyncFolder(t *testing.T) {
	e, p, w := syncFirstPair(t)
	other := filepath.Join(t.TempDir(), "elsewhere.txt")
	if err := os.WriteFile(other, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A sibling whose name merely starts with the sync folder's is outside too.
	sibling := p.LocalDir + "-copy"
	for _, path := range []string{other, sibling} {
		if res := e.SyncFirst(path); res.Kind != SyncFirstOutside {
			t.Fatalf("%s: got %+v, want SyncFirstOutside", path, res)
		}
	}
	w.nothingNudged(t)
}

func TestSyncFirstRefusesWhatIsNotSynced(t *testing.T) {
	e, p, w := syncFirstPair(t)
	if err := e.dirs.SavePairs([]config.SyncPair{{LocalDir: p.LocalDir, Excludes: []string{"Unticked"}}}); err != nil {
		t.Fatal(err)
	}
	if err := e.dirs.SaveIgnore([]string{"*.log"}); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"D/debug.log", "Unticked/inside.txt", "D/never.txt", "D/Thumbs.db"} {
		writeLocal(t, p.LocalDir, rel, "x")
	}
	if err := e.dirs.AddBlacklist(abs(p, "D/never.txt")); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"D/debug.log", "Unticked", "Unticked/inside.txt", "D/never.txt", "D/Thumbs.db"} {
		if res := e.SyncFirst(abs(p, rel)); res.Kind != SyncFirstNotSynced {
			t.Errorf("%s: got %+v, want SyncFirstNotSynced", rel, res)
		}
	}
	w.nothingNudged(t)
}

func TestSyncFirstInAFolderThatIsNotSyncing(t *testing.T) {
	e, p, w := syncFirstPair(t)
	e.SetPairGate(func(dir string) string { return "it overlaps another account's folder" })
	if res := e.SyncFirst(abs(p, "keep.txt")); res.Kind != SyncFirstHeld || res.Why == "" {
		t.Fatalf("held back: got %+v, want SyncFirstHeld with a reason", res)
	}
	e.SetPairGate(nil)

	if err := e.freezePair(p, config.Freeze{Reason: "400 files would be deleted"}); err != nil {
		t.Fatal(err)
	}
	if err := e.reloadGuardState(); err != nil {
		t.Fatal(err)
	}
	if res := e.SyncFirst(abs(p, "keep.txt")); res.Kind != SyncFirstHeld || res.Why == "" {
		t.Fatalf("paused for review: got %+v, want SyncFirstHeld with a reason", res)
	}
	w.nothingNudged(t)
}

// The menu was opened on a file that has since gone. Syncing that path now
// would send a delete; that is never the menu's to send.
func TestSyncFirstOnAPathThatHasGone(t *testing.T) {
	e, p, w := syncFirstPair(t)
	if err := os.Remove(abs(p, "keep.txt")); err != nil {
		t.Fatal(err)
	}
	if res := e.SyncFirst(abs(p, "keep.txt")); res.Kind != SyncFirstGone {
		t.Fatalf("got %+v, want SyncFirstGone", res)
	}
	w.nothingNudged(t)
}

func TestSyncFirstOnAFolder(t *testing.T) {
	e, p, w := syncFirstPair(t)
	// Settle a few files under D, so they have baseline rows.
	for _, rel := range []string{"D/same.txt", "D/edited.txt", "D/deleted.txt", "Other/elsewhere.txt"} {
		writeLocal(t, p.LocalDir, rel, "v1")
	}
	if _, err := e.SyncOnce(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	// Then: an edit, new files (one in a new folder), a delete, an ignored
	// file, and a change outside the folder.
	writeLocal(t, p.LocalDir, "D/edited.txt", "version two")
	writeLocal(t, p.LocalDir, "D/new.txt", "x")
	writeLocal(t, p.LocalDir, "D/Sub/deep.txt", "x")
	writeLocal(t, p.LocalDir, "D/Thumbs.db", "x")
	writeLocal(t, p.LocalDir, "Other/elsewhere.txt", "changed too")
	if err := os.Remove(abs(p, "D/deleted.txt")); err != nil {
		t.Fatal(err)
	}
	// Large transfers: two workers busy, then four waiting.
	r1, r2 := queueJob(t, e, p, "Other/r1.bin"), queueJob(t, e, p, "Other/r2.bin")
	waitFor(t, "the two workers to be busy", func() bool { return r1.starts.Load() == 1 && r2.starts.Load() == 1 })
	for _, rel := range []string{"Other/x.bin", "D/z.bin", "Other/y.bin", "D/Sub/a.bin"} {
		queueJob(t, e, p, rel)
	}

	res := e.SyncFirst(abs(p, "D"))
	if res.Kind != SyncFirstAsked || res.Lane != 2 || res.Paths != 4 || res.Whole {
		t.Fatalf("got %+v, want 2 large transfers moved and 4 paths handed over", res)
	}
	// The folder's waiting transfers lead, in path order; the rest keep theirs.
	if got := fmt.Sprint(waitingOrder(e)); got != "[D/Sub/a.bin D/z.bin Other/x.bin Other/y.bin]" {
		t.Fatalf("queue is %s", got)
	}
	// Changes still to be sent, and nothing else: not the file that is in
	// step, the deleted one, the ignored one, the lane's own files, or the
	// change in another folder.
	if got := fmt.Sprint(w.nudged(t, p, 4)); got != "[D/Sub D/Sub/deep.txt D/edited.txt D/new.txt]" {
		t.Fatalf("nudged %s", got)
	}
	w.nothingNudged(t)
	// The server's side of the folder is asked for as "Sync now" asks.
	if !fired(w.user) {
		t.Fatal("the folder's server-side changes were not asked for")
	}
	if fired(w.full) {
		t.Fatal("a handful of changes asked for a full pass")
	}
	if r1.starts.Load() != 1 || r2.starts.Load() != 1 {
		t.Fatal("a running transfer was interrupted")
	}
}

// A folder with more changes than a quick pass takes: sync the whole sync
// folder instead of dropping the surplus.
func TestSyncFirstOnAFolderWithTooManyChanges(t *testing.T) {
	e, p, w := syncFirstPair(t)
	for i := 0; i <= maxSyncPaths; i++ {
		writeLocal(t, p.LocalDir, fmt.Sprintf("D/f%04d.txt", i), "x")
	}
	res := e.SyncFirst(abs(p, "D"))
	if res.Kind != SyncFirstAsked || !res.Whole || res.Paths != 0 {
		t.Fatalf("got %+v, want the whole sync folder asked for", res)
	}
	if !fired(w.full) {
		t.Fatal("no full pass was asked for")
	}
	w.nothingNudged(t)
}

// The sync folder itself: everything in it that is still to be sent.
func TestSyncFirstOnTheSyncFolderItself(t *testing.T) {
	e, p, w := syncFirstPair(t)
	writeLocal(t, p.LocalDir, "D/new.txt", "x")
	res := e.SyncFirst(p.LocalDir)
	if res.Kind != SyncFirstAsked || res.Paths != 1 {
		t.Fatalf("got %+v, want one path", res)
	}
	if got := fmt.Sprint(w.nudged(t, p, 1)); got != "[D/new.txt]" {
		t.Fatalf("nudged %s", got)
	}
}

// Before the engine runs (the app was started by the menu) there is no
// watcher to hand anything to. The request still counts for the lane.
func TestSyncFirstBeforeTheFolderIsWatched(t *testing.T) {
	_, _, e, p := lanePair(t, "PUT", "never")
	if err := e.dirs.SavePairs([]config.SyncPair{{LocalDir: p.LocalDir}}); err != nil {
		t.Fatal(err)
	}
	e.transferLane().pause()
	queueJob(t, e, p, "D/a.bin")
	if res := e.SyncFirst(abs(p, "keep.txt")); res.Kind != SyncFirstAsked || res.Paths != 0 {
		t.Fatalf("got %+v, want nothing handed over", res)
	}
	queueJob(t, e, p, "keep.txt")
	if got := fmt.Sprint(waitingOrder(e)); got != "[keep.txt D/a.bin]" {
		t.Fatalf("queue is %s", got)
	}
}

// runWatcher starts the pair's real watcher on e, as Run does, and stops it
// when the test ends. Nothing polls: only what the test asks for syncs.
func runWatcher(t *testing.T, e *Engine, p Pair) {
	t.Helper()
	runCtx, cancel := context.WithCancel(context.Background())
	// With push "available" a pass does not poll for notifications afterwards
	// (this engine has no notifier). Nothing connects to it: Run is not used.
	e.caps = &transport.Capabilities{NotifyPush: &transport.NotifyPush{}}
	e.pollInterval = time.Hour
	e.watchMu.Lock()
	e.runCtx = runCtx
	e.watchers = map[string]context.CancelFunc{}
	e.triggers, e.triggersFull, e.triggersUser = map[string]chan struct{}{}, map[string]chan struct{}{}, map[string]chan struct{}{}
	e.nudges, e.watchDone = map[string]chan string{}, map[string]chan struct{}{}
	e.watchMu.Unlock()
	e.startWatcher(p)
	e.watchMu.Lock()
	done := e.watchDone[PairKey(p.LocalDir, p.RemoteRoot)]
	e.watchMu.Unlock()
	if done == nil {
		t.Fatal("the watcher did not start")
	}
	t.Cleanup(func() { cancel(); <-done })
}

// backOff makes the pair's next server-walking pass fail, so the watcher is
// backing off (for an hour) when this returns, then lets the server answer
// again.
func backOff(t *testing.T, f *fakeDAV, e *Engine) {
	t.Helper()
	before := f.pfCount("")
	f.setFailPF("", 403)
	e.TriggerSync()
	waitFor(t, "the failing pass", func() bool { return f.pfCount("") > before })
	time.Sleep(100 * time.Millisecond) // let the pass return and the hold begin
	f.clearFailPF("")
}

func localIs(p Pair, rel, want string) func() bool {
	return func() bool {
		b, err := os.ReadFile(abs(p, rel))
		return err == nil && string(b) == want
	}
}

// The whole route, with the pair's real watcher: a file and then a folder
// asked for while a failed pass is being backed off from are synced at once.
func TestSyncFirstReachesTheServerDuringABackoff(t *testing.T) {
	f, _, e, p := lanePair(t, "PUT", "never")
	if err := e.dirs.SavePairs([]config.SyncPair{{LocalDir: p.LocalDir}}); err != nil {
		t.Fatal(err)
	}
	startup := f.pfCount("")
	runWatcher(t, e, p)
	waitFor(t, "the watcher's first pass", func() bool { return f.pfCount("") > startup })

	// A file. Its new version on the server is not fetched while the watcher
	// backs off...
	backOff(t, f, e)
	f.setNode("keep.txt", davNode{etag: "e-keep2", body: "version two"})
	f.setNode("", davNode{isDir: true, etag: "e-root2"})
	e.TriggerSync()
	time.Sleep(900 * time.Millisecond) // past the watcher's half-second debounce
	if !localIs(p, "keep.txt", "k")() {
		t.Fatal("the backoff did not hold: the test proves nothing")
	}
	// ...until it is asked for by name.
	if res := e.SyncFirst(abs(p, "keep.txt")); res.Kind != SyncFirstAsked || res.Paths != 1 {
		t.Fatalf("file: got %+v", res)
	}
	waitFor(t, "the file's new version", localIs(p, "keep.txt", "version two"))

	// A folder. What changed in it on the server comes down too.
	backOff(t, f, e)
	f.setNode("D/from-server.txt", davNode{etag: "e-new", body: "hello"})
	f.setNode("D", davNode{isDir: true, etag: "e-d2"})
	f.setNode("", davNode{isDir: true, etag: "e-root3"})
	if res := e.SyncFirst(abs(p, "D")); res.Kind != SyncFirstAsked {
		t.Fatalf("folder: got %+v", res)
	}
	waitFor(t, "the folder's new file", localIs(p, "D/from-server.txt", "hello"))
}

func TestLanePreferRunsOut(t *testing.T) {
	l := newLane(true)
	t.Cleanup(l.close)
	l.add(newTestJob("pk", "a").j)
	l.prefer(filepath.Join(`C:\Sync`, "late"), time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	l.add(newTestJob("pk", "late").j)
	if ents := l.entries(); ents[0].rel != "a" || ents[1].rel != "late" {
		t.Fatalf("a request that ran out still moved its file to the front: %s, %s", ents[0].rel, ents[1].rel)
	}
}

func TestLanePreferKeepsAFoldersFilesInOrder(t *testing.T) {
	l := newLane(true)
	t.Cleanup(l.close)
	l.add(newTestJob("pk", "other").j)
	// Joined like newTestJob's paths: on Linux a backslash is no separator, so
	// a literal `C:\Sync\F` never contains `C:\Sync/F/1` (CI, 2026-10-13).
	l.prefer(filepath.Join(`C:\Sync`, "F"), time.Minute)
	for _, rel := range []string{"F/1", "F/2", "Fother", "F/3"} {
		l.add(newTestJob("pk", rel).j)
	}
	var got []string
	for _, en := range l.entries() {
		got = append(got, en.rel)
	}
	if fmt.Sprint(got) != "[F/1 F/2 F/3 other Fother]" {
		t.Fatalf("queue is %v", got)
	}
}
