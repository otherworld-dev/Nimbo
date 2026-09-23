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
