package agent

// The long-transfer lane driven through real sync passes (Deck #702). A
// gatedDAV holds one file's transfer open, like a big file on a slow line, so
// the tests can check what the rest of the sync does meanwhile.

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/otherworld/nimbo/internal/engine"
)

// gatedDAV wraps fakeDAV so a transfer of one path hangs until open is
// called. hits counts its requests; cancelled closes when the client gives up
// on one (a stop, a pause, a quit).
type gatedDAV struct {
	*fakeDAV
	method, path string
	release      chan struct{}
	cancelled    chan struct{}
	openOnce     sync.Once
	cancelOnce   sync.Once
	hits         atomic.Int32
}

func newGatedDAV(f *fakeDAV, method, path string) *gatedDAV {
	return &gatedDAV{fakeDAV: f, method: method, path: path,
		release: make(chan struct{}), cancelled: make(chan struct{})}
}

// open lets the held transfer, and any later one of the path, through.
func (g *gatedDAV) open() { g.openOnce.Do(func() { close(g.release) }) }

func (g *gatedDAV) hasStarted() bool { return g.hits.Load() > 0 }

func (g *gatedDAV) wasCancelled() bool {
	select {
	case <-g.cancelled:
		return true
	default:
		return false
	}
}

func (g *gatedDAV) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rel := strings.Trim(strings.TrimPrefix(r.URL.Path, davPrefix), "/")
	if r.Method == g.method && rel == g.path {
		// Read the body first: the server only notices a client hanging up
		// once the request body is in.
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		g.hits.Add(1)
		select {
		case <-g.release:
		case <-r.Context().Done():
			g.cancelOnce.Do(func() { close(g.cancelled) })
			return
		}
	}
	g.fakeDAV.ServeHTTP(w, r)
}

// lanePair builds a settled pair (a folder D beside keep.txt) served through
// a gate on one path, with the lane threshold lowered so an 8 KiB file is
// "large".
func lanePair(t *testing.T, method, path string) (*fakeDAV, *gatedDAV, *Engine, Pair) {
	t.Helper()
	old := laneMinBytes
	laneMinBytes = 4 << 10
	t.Cleanup(func() { laneMinBytes = old })
	f := newFakeDAV(map[string]davNode{
		"":         {isDir: true, etag: "e-root"},
		"D":        {isDir: true, etag: "e-d"},
		"keep.txt": {etag: "e-keep", body: "k"},
	})
	g := newGatedDAV(f, method, path)
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	t.Cleanup(g.open) // before srv.Close, which waits for handlers
	e, _ := newHookEngine(t, srv.URL)
	t.Cleanup(e.closeLane) // cleanups run in reverse: the lane stops before the store closes
	p := Pair{LocalDir: t.TempDir()}
	for _, pass := range []string{"seed", "settle"} {
		if _, err := e.SyncOnce(context.Background(), p); err != nil {
			t.Fatalf("%s sync: %v", pass, err)
		}
	}
	return f, g, e, p
}

func statusOf(e *Engine) string {
	e.diagMu.Lock()
	defer e.diagMu.Unlock()
	return e.lastStatus
}

var bigBody = strings.Repeat("x", 8<<10) // "large" once lanePair lowers the threshold

// The card itself: a small file saved while a large upload runs syncs at once.
func TestALargeUploadDoesNotHoldUpTheNextPass(t *testing.T) {
	f, g, e, p := lanePair(t, "PUT", "D/big.bin")
	writeLocal(t, p.LocalDir, "D/big.bin", bigBody)
	var err error
	returnsSoon(t, "the pass that met the large file", func() { _, err = e.SyncOnce(context.Background(), p) })
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the large transfer to start", g.hasStarted)
	if got := statusOf(e); got != "Syncing 1 large file…" {
		t.Errorf("status = %q while the large file uploads", got)
	}

	writeLocal(t, p.LocalDir, "D/note.txt", "urgent")
	returnsSoon(t, "the next pass", func() { _, err = e.SyncPaths(context.Background(), p, []string{"D/note.txt"}) })
	if err != nil {
		t.Fatal(err)
	}
	if got := f.putBody("D/note.txt"); got != "urgent" {
		t.Fatalf("the small file waited behind the large one: server has %q", got)
	}

	g.open()
	waitFor(t, "the lane to finish", func() bool { return e.transferLane().count() == 0 })
	if got := f.putBody("D/big.bin"); got != bigBody {
		t.Fatalf("large file on the server is %d bytes, want %d", len(got), len(bigBody))
	}
	// The count drops just before the job's done runs, so wait for the line.
	waitFor(t, "the status to move on from the large file", func() bool {
		return !strings.Contains(statusOf(e), "large file")
	})
}

// Saving the big file again while it uploads must not start a second upload.
func TestASecondPassLeavesALaneUploadAlone(t *testing.T) {
	_, g, e, p := lanePair(t, "PUT", "D/big.bin")
	writeLocal(t, p.LocalDir, "D/big.bin", bigBody)
	var err error
	returnsSoon(t, "the first pass", func() { _, err = e.SyncOnce(context.Background(), p) })
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the large transfer to start", g.hasStarted)
	returnsSoon(t, "the second pass", func() { _, err = e.SyncPaths(context.Background(), p, []string{"D/big.bin"}) })
	if err != nil {
		t.Fatal(err)
	}
	if n := g.hits.Load(); n != 1 {
		t.Fatalf("%d uploads of the large file started, want 1", n)
	}
}

// A quit drops a lane download. The pass that handed it over must not have
// stamped its folder settled, or the next start never looks there again
// (the Deck #691 lesson).
func TestAQuitMidDownloadLeavesItsFolderToBeScannedAgain(t *testing.T) {
	f, g, e, p := lanePair(t, "GET", "D/big.bin")
	f.setNode("D/big.bin", davNode{etag: "e-big", body: bigBody})
	f.setNode("D", davNode{isDir: true, etag: "e-d2"})
	f.setNode("", davNode{isDir: true, etag: "e-root2"})
	var err error
	returnsSoon(t, "the pass that met the large file", func() { _, err = e.SyncOnce(context.Background(), p) })
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the large transfer to start", g.hasStarted)
	e.closeLane() // quit
	g.open()

	if _, err := e.SyncOnce(context.Background(), p); err != nil {
		t.Fatalf("the pass after restart: %v", err)
	}
	waitFor(t, "the lane to finish", func() bool { return e.transferLane().count() == 0 })
	got, rerr := os.ReadFile(filepath.Join(p.LocalDir, "D", "big.bin"))
	if rerr != nil || string(got) != bigBody {
		t.Fatalf("the large file was never fetched after the quit (%v): its folder was stamped settled", rerr)
	}
}

// Stopping the engine stops its lane. Android stops and restarts the engine
// in a live process (sign-out, account change, the sync service going away),
// so a lane that outlived Run finished its download against a closed state
// DB: the file landed with no baseline, and the dead engine said "Up to
// date" through the listener it was given.
func TestStoppingTheEngineStopsItsLaneTransfers(t *testing.T) {
	f, g, e, p := lanePair(t, "GET", "D/big.bin")
	f.setNode("D/big.bin", davNode{etag: "e-big", body: bigBody})
	f.setNode("D", davNode{isDir: true, etag: "e-d2"})
	f.setNode("", davNode{isDir: true, etag: "e-root2"})
	var err error
	returnsSoon(t, "the pass that met the large file", func() { _, err = e.SyncOnce(context.Background(), p) })
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the large transfer to start", g.hasStarted)

	returnsSoon(t, "the engine stopping", e.closeRun) // what Run does as it returns
	if !g.wasCancelled() {
		t.Fatal("the large download was still running after the engine stopped")
	}
	if n := e.transferLane().count(); n != 0 {
		t.Fatalf("%d lane transfers left after the engine stopped, want 0", n)
	}
	if _, serr := os.Stat(filepath.Join(p.LocalDir, "D", "big.bin")); !os.IsNotExist(serr) {
		t.Fatalf("the large file was written after the engine stopped (%v)", serr)
	}
}

// The server deletes the folder of a large download mid-transfer: the
// download is stopped first, then the folder goes, and the pass doesn't hang.
func TestAFolderDeletedOnTheServerStopsItsLaneDownload(t *testing.T) {
	f, g, e, p := lanePair(t, "GET", "D/big.bin")
	f.setNode("D/big.bin", davNode{etag: "e-big", body: bigBody})
	f.setNode("D", davNode{isDir: true, etag: "e-d2"})
	f.setNode("", davNode{isDir: true, etag: "e-root2"})
	var err error
	returnsSoon(t, "the pass that met the large file", func() { _, err = e.SyncOnce(context.Background(), p) })
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the large transfer to start", g.hasStarted)

	f.delNode("D/big.bin")
	f.delNode("D")
	f.setNode("", davNode{isDir: true, etag: "e-root3"})
	returnsSoon(t, "the pass deleting the folder", func() { _, err = e.SyncOnce(context.Background(), p) })
	if err != nil {
		t.Fatal(err)
	}
	if n := e.transferLane().count(); n != 0 {
		t.Fatalf("lane still holds %d transfers after their folder was deleted", n)
	}
	waitFor(t, "the server to see the download cancelled", g.wasCancelled)
	if _, serr := os.Stat(filepath.Join(p.LocalDir, "D")); !os.IsNotExist(serr) {
		t.Fatalf("folder deleted on the server is still here: %v", serr)
	}
}

// Pause stops the large transfer at once; nothing restarts while paused;
// resume carries on.
func TestPauseStopsALaneUploadAndResumeFinishesIt(t *testing.T) {
	f, g, e, p := lanePair(t, "PUT", "D/big.bin")
	writeLocal(t, p.LocalDir, "D/big.bin", bigBody)
	var err error
	returnsSoon(t, "the pass", func() { _, err = e.SyncOnce(context.Background(), p) })
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the large transfer to start", g.hasStarted)

	e.SetPaused(true)
	waitFor(t, "the upload to stop on pause", g.wasCancelled)
	if n := e.transferLane().count(); n != 1 {
		t.Fatalf("lane holds %d, want the paused upload kept", n)
	}
	time.Sleep(100 * time.Millisecond)
	if n := g.hits.Load(); n != 1 {
		t.Fatalf("upload restarted while paused (%d starts)", n)
	}

	g.open()
	e.SetPaused(false)
	waitFor(t, "the lane to finish after resume", func() bool { return e.transferLane().count() == 0 })
	if got := f.putBody("D/big.bin"); got != bigBody {
		t.Fatalf("large file on the server is %d bytes after resume, want %d", len(got), len(bigBody))
	}
}

func TestBlacklistingALaneFileStopsItsUpload(t *testing.T) {
	f, g, e, p := lanePair(t, "PUT", "D/big.bin")
	writeLocal(t, p.LocalDir, "D/big.bin", bigBody)
	var err error
	returnsSoon(t, "the pass", func() { _, err = e.SyncOnce(context.Background(), p) })
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the large transfer to start", g.hasStarted)
	returnsSoon(t, "blacklisting", func() { err = e.BlacklistPath(filepath.Join(p.LocalDir, "D", "big.bin")) })
	if err != nil {
		t.Fatal(err)
	}
	if n := e.transferLane().count(); n != 0 {
		t.Fatalf("lane still holds %d after the file was blacklisted", n)
	}
	waitFor(t, "the upload to be cancelled", g.wasCancelled)
	if got := f.putBody("D/big.bin"); got != "" {
		t.Fatal("a blacklisted file reached the server")
	}
}

// Removing or moving a sync folder stops its watcher with stopWatcher(Sync);
// its lane transfers must be stopped and waited for too.
func TestStoppingAFoldersWatcherStopsItsLaneTransfers(t *testing.T) {
	_, g, e, p := lanePair(t, "PUT", "D/big.bin")
	writeLocal(t, p.LocalDir, "D/big.bin", bigBody)
	var err error
	returnsSoon(t, "the pass", func() { _, err = e.SyncOnce(context.Background(), p) })
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the large transfer to start", g.hasStarted)
	returnsSoon(t, "stopping the folder", func() { e.stopWatcherSync(PairKey(p.LocalDir, p.RemoteRoot)) })
	if n := e.transferLane().count(); n != 0 {
		t.Fatalf("lane still holds %d for a folder that stopped syncing", n)
	}
	waitFor(t, "the upload to be cancelled", g.wasCancelled)
}

func progRunsOf(e *Engine) int {
	e.progMu.Lock()
	defer e.progMu.Unlock()
	return e.progRuns
}

// Set aside mid-upload: the upload stops, the status says so, a pass leaves
// the file alone, and Resume takes it out of the lane for a fresh pass, which
// then uploads it. The progress burst ends once, not twice.
func TestSettingALaneUploadAsideStopsItAndResumeReplansIt(t *testing.T) {
	f, g, e, p := lanePair(t, "PUT", "D/big.bin")
	writeLocal(t, p.LocalDir, "D/big.bin", bigBody)
	var err error
	returnsSoon(t, "the pass", func() { _, err = e.SyncOnce(context.Background(), p) })
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the large transfer to start", g.hasStarted)
	abs := filepath.Join(p.LocalDir, "D", "big.bin")

	ents := e.LaneEntries()
	if len(ents) != 1 || ents[0].State != "running" || !ents[0].Upload || ents[0].Size != int64(len(bigBody)) ||
		ents[0].Path != "D/big.bin" || ents[0].LocalDir != p.LocalDir {
		t.Fatalf("entries = %+v", ents)
	}

	e.progStart(1, 0) // a sentinel burst: a second progEnd would take it away
	t.Cleanup(e.progEnd)
	if !e.LaneSetAside(abs, time.Time{}) {
		t.Fatal("LaneSetAside refused the running upload")
	}
	waitFor(t, "the upload to stop", g.wasCancelled)
	waitFor(t, "the set-aside status", func() bool { return statusOf(e) == "1 large file set aside" })
	if n := progRunsOf(e); n != 1 {
		t.Fatalf("progress runs = %d after setting aside, want 1 (the sentinel)", n)
	}
	if ents := e.LaneEntries(); len(ents) != 1 || ents[0].State != "setaside" {
		t.Fatalf("entries after set aside = %+v", ents)
	}

	returnsSoon(t, "a pass over the set-aside file", func() { _, err = e.SyncPaths(context.Background(), p, []string{"D/big.bin"}) })
	if err != nil {
		t.Fatal(err)
	}
	if n := g.hits.Load(); n != 1 {
		t.Fatalf("a pass started the set-aside upload again (%d starts)", n)
	}

	if !e.LaneResume(abs) {
		t.Fatal("LaneResume refused the set-aside upload")
	}
	if n := progRunsOf(e); n != 1 {
		t.Fatalf("progress runs = %d after resume, want 1: the job's burst ended twice", n)
	}
	if e.transferLane().count() != 0 || e.transferLane().parkedCount() != 0 {
		t.Fatal("the lane still holds the file after Resume")
	}
	waitFor(t, "the status to move on", func() bool { return !strings.Contains(statusOf(e), "large file") })

	g.open()
	if _, err := e.SyncOnce(context.Background(), p); err != nil { // what the nudge's pass does
		t.Fatal(err)
	}
	waitFor(t, "the lane to finish", func() bool { return e.transferLane().count() == 0 })
	if got := f.putBody("D/big.bin"); got != bigBody {
		t.Fatalf("large file on the server is %d bytes, want %d", len(got), len(bigBody))
	}
}

// A set-aside download's folder must not be stamped settled: after a quit
// the next start must still fetch it (the #691 lesson).
func TestASetAsideDownloadLeavesItsFolderToBeScannedAgain(t *testing.T) {
	f, g, e, p := lanePair(t, "GET", "D/big.bin")
	f.setNode("D/big.bin", davNode{etag: "e-big", body: bigBody})
	f.setNode("D", davNode{isDir: true, etag: "e-d2"})
	f.setNode("", davNode{isDir: true, etag: "e-root2"})
	var err error
	returnsSoon(t, "the pass that met the large file", func() { _, err = e.SyncOnce(context.Background(), p) })
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the large transfer to start", g.hasStarted)
	if !e.LaneSetAside(filepath.Join(p.LocalDir, "D", "big.bin"), time.Now().Add(time.Hour)) {
		t.Fatal("LaneSetAside refused the running download")
	}
	waitFor(t, "the download to stop", g.wasCancelled)
	if _, err := e.SyncOnce(context.Background(), p); err != nil { // a full pass while it is set aside
		t.Fatal(err)
	}
	e.closeLane() // quit
	g.open()

	if _, err := e.SyncOnce(context.Background(), p); err != nil {
		t.Fatalf("the pass after restart: %v", err)
	}
	waitFor(t, "the lane to finish", func() bool { return e.transferLane().count() == 0 })
	got, rerr := os.ReadFile(filepath.Join(p.LocalDir, "D", "big.bin"))
	if rerr != nil || string(got) != bigBody {
		t.Fatalf("the set-aside file was never fetched after the quit (%v): its folder was stamped settled", rerr)
	}
}

// The seed/settle passes in lanePair already create the lane (transferLane is
// called from the first SyncOnce), so SetLaneFunc below is registering onto
// an existing lane, not a future one; transferLane's onChange closure reads
// e.onLane at call time, which is why setting the hook after the lane exists
// still reaches it.
func TestLaneChangesReachTheListener(t *testing.T) {
	_, g, e, p := lanePair(t, "PUT", "D/big.bin")
	var calls atomic.Int32
	e.SetLaneFunc(func() { calls.Add(1) })
	writeLocal(t, p.LocalDir, "D/big.bin", bigBody)
	var err error
	returnsSoon(t, "the pass", func() { _, err = e.SyncOnce(context.Background(), p) })
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the large transfer to start", g.hasStarted)
	waitFor(t, "a lane change to be reported", func() bool { return calls.Load() > 0 })
}

func inflightOf(e *Engine, abs string) bool {
	e.inflightMu.Lock()
	defer e.inflightMu.Unlock()
	return e.inflight[abs]
}

// A late onPark must be a no-op (ruling 1, Task 3). The lane can call a
// job's onPark AFTER its done: exec() parks a cancelled running job and
// unlocks before calling onPark, and in that gap a concurrent unpark/stop/
// timer can find the job already on the parked list and call done first.
// Without the left guard, that late onPark would run laneParked and clear
// the inflight mark and lock-warning retake of a fresh job a pass has
// already re-planned and re-sent for the same path. This reproduces the
// race deterministically by calling the job's own closures directly, in the
// done-then-onPark order the race can produce, without ever running its
// transfer (the lane is paused so sendToLane's job only ever queues).
func TestALateOnParkAfterDoneDoesNothing(t *testing.T) {
	e, _ := newHookEngine(t, "http://example.invalid")
	p := Pair{LocalDir: t.TempDir()}
	pk := PairKey(p.LocalDir, p.RemoteRoot)
	abs := filepath.Join(p.LocalDir, "D", "big.bin")

	e.SetPaused(true) // transferLane() below creates the lane paused: the job queues, never runs
	if !e.sendToLane(p, pk, engine.Action{Kind: engine.ActUpload, Path: "D/big.bin"}, 8<<10, nil) {
		t.Fatal("sendToLane refused the job")
	}
	l := e.transferLane()
	l.mu.Lock()
	if len(l.queue) != 1 {
		l.mu.Unlock()
		t.Fatalf("queue = %d, want 1", len(l.queue))
	}
	j := l.queue[0]
	l.mu.Unlock()

	if !inflightOf(e, abs) {
		t.Fatal("sendToLane did not mark the path inflight")
	}
	if n := progRunsOf(e); n != 1 {
		t.Fatalf("progRuns = %d after sendToLane, want 1", n)
	}

	// The race: a concurrent stop (standing in for stop/unpark/a timer) takes
	// the job and calls done before the lane calls onPark for the same job.
	j.done(errLaneStopped)
	if inflightOf(e, abs) {
		t.Fatal("done did not clear inflight")
	}
	if n := progRunsOf(e); n != 0 {
		t.Fatalf("progRuns = %d after done, want 0", n)
	}

	// A fresh pass re-plans and re-sends the same path: a new job takes over
	// the inflight mark and starts a new progress burst.
	e.markInflight(abs, true)
	e.progStart(1, 0)

	// The stale onPark, arriving late, must leave the fresh job's bookkeeping
	// alone.
	j.onPark()
	if !inflightOf(e, abs) {
		t.Fatal("a late onPark cleared the fresh job's inflight mark")
	}
	if n := progRunsOf(e); n != 1 {
		t.Fatalf("progRuns = %d after the late onPark, want 1: it touched the fresh job's progress burst", n)
	}
	e.progEnd() // balance the manual progStart above
}

// Finding A (Task 3 review, fix round 1): setting a running lane upload
// aside must retract its share of the burst's totals, or the flyout's % and
// ETA stay computed against a file that has stopped counting toward Done and
// will never reach Total (the #702 spec: a set-aside file's share of the
// progress bar ends).
func TestSettingALaneUploadAsideDropsItsShareOfTheTotals(t *testing.T) {
	_, g, e, p := lanePair(t, "PUT", "D/big.bin")
	writeLocal(t, p.LocalDir, "D/big.bin", bigBody)
	var err error
	returnsSoon(t, "the pass", func() { _, err = e.SyncOnce(context.Background(), p) })
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the large transfer to start", g.hasStarted)
	abs := filepath.Join(p.LocalDir, "D", "big.bin")

	e.progStart(1, 0) // a sentinel burst: keeps progRuns > 0 across the set-aside
	t.Cleanup(e.progEnd)
	before := e.Progress()
	if !e.LaneSetAside(abs, time.Time{}) {
		t.Fatal("LaneSetAside refused the running upload")
	}
	waitFor(t, "the totals to drop by the set-aside file's share", func() bool {
		p := e.Progress()
		return p.Total == before.Total-1 && p.TotalBytes == before.TotalBytes-int64(len(bigBody))
	})
	// Give a wrong retraction (e.g. one that fires twice, or drifts) a chance
	// to show up before declaring it settled.
	time.Sleep(50 * time.Millisecond)
	after := e.Progress()
	if after.Total != before.Total-1 || after.TotalBytes != before.TotalBytes-int64(len(bigBody)) {
		t.Fatalf("Progress = %+v, want Total=%d TotalBytes=%d", after, before.Total-1, before.TotalBytes-int64(len(bigBody)))
	}
}

// Finding B (Task 3 review, fix round 1): done(errLaneUnparked) must clear
// inflight itself, because onPark can lose the race to done (see
// TestALateOnParkAfterDoneDoesNothing) — a timer whose until is already
// past, or a quick LaneResume, can call done before the lane ever gets
// around to calling onPark for the same job. If nothing else clears it and
// the re-plan that follows finds nothing to do (the file was deleted, or
// already matches), an in-sync file would show the syncing overlay forever.
func TestDoneUnparkedClearsInflightEvenIfOnParkNeverRuns(t *testing.T) {
	e, _ := newHookEngine(t, "http://example.invalid")
	p := Pair{LocalDir: t.TempDir()}
	pk := PairKey(p.LocalDir, p.RemoteRoot)
	abs := filepath.Join(p.LocalDir, "D", "big.bin")

	e.SetPaused(true) // transferLane() below creates the lane paused: the job queues, never runs
	if !e.sendToLane(p, pk, engine.Action{Kind: engine.ActUpload, Path: "D/big.bin"}, 8<<10, nil) {
		t.Fatal("sendToLane refused the job")
	}
	l := e.transferLane()
	l.mu.Lock()
	j := l.queue[0]
	l.mu.Unlock()
	if !inflightOf(e, abs) {
		t.Fatal("sendToLane did not mark the path inflight")
	}

	j.done(errLaneUnparked) // onPark never runs: nothing else may clear inflight
	if inflightOf(e, abs) {
		t.Fatal("done(errLaneUnparked) left the path marked inflight")
	}

	// A late onPark, arriving after all, must still be the no-op ruling 1
	// requires — not because it is needed here (done already cleared
	// inflight), but to confirm it doesn't reach back and touch a fresh
	// job's state that took the path over meanwhile.
	e.markInflight(abs, true)
	j.onPark()
	if !inflightOf(e, abs) {
		t.Fatal("a late onPark cleared a fresh job's inflight mark")
	}
}

// A folder removed while a pass of it is between planning and the lane (the
// watcher is cancelled, not waited for): that pass must neither hand the lane
// its large download nor fetch it itself, or the download recreates the
// folder the user asked to delete. Once the folder syncs again (its watcher
// starts: re-added, or moved back), the lane takes its transfers again.
func TestAPassOfAStoppedFolderLeavesItsLargeTransfersAlone(t *testing.T) {
	f, g, e, p := lanePair(t, "GET", "D/big.bin")
	f.setNode("D/big.bin", davNode{etag: "e-big", body: bigBody})
	f.setNode("D", davNode{isDir: true, etag: "e-d2"})
	f.setNode("", davNode{isDir: true, etag: "e-root2"})
	pk := PairKey(p.LocalDir, p.RemoteRoot)
	local := filepath.Join(p.LocalDir, "D", "big.bin")

	e.stopLanePair(pk) // as stopWatcher does while the pass below is planning
	var err error
	returnsSoon(t, "the pass", func() { _, err = e.SyncOnce(context.Background(), p) })
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // give a stray lane job time to reach the server
	if n := g.hits.Load(); n != 0 {
		t.Fatalf("a folder that stopped syncing still fetched its large file (%d GETs)", n)
	}
	if e.transferLane().has(pk, "D/big.bin") {
		t.Fatal("the lane took a transfer for a folder that stopped syncing")
	}
	if _, serr := os.Stat(local); serr == nil {
		t.Fatal("the large file was downloaded into a folder that stopped syncing")
	}

	// The folder syncs again: its watcher starts (on a run that is already
	// over, so it stops again at once and the test drives the pass itself).
	runCtx, cancel := context.WithCancel(context.Background())
	cancel()
	e.watchMu.Lock()
	e.runCtx = runCtx
	e.watchers, e.triggers, e.triggersFull = map[string]context.CancelFunc{}, map[string]chan struct{}{}, map[string]chan struct{}{}
	e.nudges, e.watchDone = map[string]chan string{}, map[string]chan struct{}{}
	e.watchMu.Unlock()
	e.startWatcher(p)
	e.watchMu.Lock()
	done := e.watchDone[pk]
	e.watchMu.Unlock()
	if done != nil {
		<-done
	}
	if e.transferLane().pairStopped(pk) {
		t.Fatal("starting the folder's watcher left the lane refusing it")
	}
	g.open()
	returnsSoon(t, "the pass", func() { _, err = e.SyncOnce(context.Background(), p) })
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the large download to land", func() bool {
		b, rerr := os.ReadFile(local)
		return rerr == nil && string(b) == bigBody
	})
}
