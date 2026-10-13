package agent

// Download these first (Deck #766): the clone's work queue, and the order a
// real clone (SyncOnce against fakeDAV) downloads in when folders are chosen
// before it starts and while it runs.

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/otherworld/nimbo/internal/engine"
	"github.com/otherworld/nimbo/internal/transport"
)

func dl(paths ...string) []engine.Action {
	out := make([]engine.Action, 0, len(paths))
	for _, p := range paths {
		out = append(out, engine.Action{Kind: engine.ActDownload, Path: p})
	}
	return out
}

// drain takes every queued file off the queue, in the order handed out.
func drain(t *testing.T, s *cloneSched) []string {
	t.Helper()
	var got []string
	for {
		a, _, ok := s.nextDownload()
		if !ok {
			return got
		}
		got = append(got, a.Path)
	}
}

func TestNormCloneFirst(t *testing.T) {
	got := normCloneFirst([]string{"/Work/Current/", `Photos\2024`, "", "  ", "Work/Current", "../x", "a/../b", "Music"})
	want := []string{"Work/Current", "Photos/2024", "Music"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normCloneFirst = %v, want %v", got, want)
	}
}

// With nothing chosen the queue keeps the order folders were queued in, files
// sorted within each folder; a chosen folder (and everything under it) goes
// first, ranked by its place in the choice.
func TestCloneSchedChosenFolderFirst(t *testing.T) {
	s := newCloneSched("", nil)
	s.setFirst([]string{"C", "B/deep"})
	s.queueDownloads(dl("A/a2", "A/a1"), nil)
	s.queueDownloads(dl("B/b1", "B/deep/d1"), nil)
	s.queueDownloads(dl("C/z", "C/sub/y", "C/x"), nil)

	got := drain(t, s)
	want := []string{"C/x", "C/z", "C/sub/y", "B/deep/d1", "A/a1", "A/a2", "B/b1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v\nwant  %v", got, want)
	}
}

// Changing the choice while files are queued re-ranks what is still waiting.
func TestCloneSchedReprioritiseMidRun(t *testing.T) {
	s := newCloneSched("", nil)
	s.queueDownloads(dl("A/a1", "A/a2", "A/a3"), nil)
	s.queueDownloads(dl("B/b1"), nil)
	s.queueDownloads(dl("C/c1", "C/c2"), nil)

	a, _, _ := s.nextDownload()
	if a.Path != "A/a1" {
		t.Fatalf("first = %s, want A/a1", a.Path)
	}
	s.setFirst([]string{"C"})
	got := drain(t, s)
	want := []string{"C/c1", "C/c2", "A/a2", "A/a3", "B/b1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("after choosing C = %v\nwant %v", got, want)
	}

	// And back: clearing the choice returns to the queued order.
	s2 := newCloneSched("", nil)
	s2.setFirst([]string{"C"})
	s2.queueDownloads(dl("A/a1"), nil)
	s2.queueDownloads(dl("C/c1"), nil)
	s2.setFirst(nil)
	if got := drain(t, s2); !reflect.DeepEqual(got, []string{"A/a1", "C/c1"}) {
		t.Fatalf("after clearing = %v, want [A/a1 C/c1]", got)
	}
}

// A chosen folder inside a top-level folder whose listing is not planned yet
// is listed on its own, first, and the top-level listing leaves its contents
// to it. Once a listing is planned, nothing more is carved out of it.
func TestCloneSchedCarvesNestedChoice(t *testing.T) {
	s := newCloneSched("Root", func(p string) bool { return p == "Big/Ignored" })
	s.addListing("Big")
	s.addListing("Other")
	s.setFirst([]string{"Big/Sub", "Big/Sub/Deeper", "Other", "Big/Ignored", "Nowhere/x"})

	// Big/Sub is carved; Big/Sub/Deeper is inside a carve that is not planned
	// either, so it is carved from that; Other IS a listing (no carve); the
	// ignored folder and a folder in no listing are not.
	var order []string
	var big *cloneListing
	for i := 0; i < 4; i++ {
		l := s.nextListing()
		if l == nil {
			t.Fatalf("listing %d: none", i)
		}
		if l.rel == "Big" {
			big = l
		}
		if l.rel == "Big/Sub" && l.full != "Root/Big/Sub" {
			t.Errorf("carve lists %q, want Root/Big/Sub", l.full)
		}
		order = append(order, l.rel)
	}
	want := []string{"Big/Sub", "Big/Sub/Deeper", "Other", "Big"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("listing order = %v, want %v", order, want)
	}

	carved := s.seal(big)
	if !reflect.DeepEqual(carved, []string{"Big/Sub", "Big/Sub/Deeper"}) {
		t.Fatalf("carved out of Big = %v", carved)
	}
	// Big is planned now: a new choice inside it reorders, never carves.
	s.setFirst([]string{"Big/Later"})
	s.mu.Lock()
	n := len(s.pending)
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("a choice inside a planned listing was carved (%d waiting)", n)
	}
	for i := 0; i < 4; i++ {
		s.listingDone()
	}
	if l := s.nextListing(); l != nil {
		t.Fatalf("listing after all done = %v", l.rel)
	}
}

// A download worker waits while a listing is still running (it may queue
// more), and leaves once nothing is queued and nothing is listing.
func TestCloneSchedWaitsForListings(t *testing.T) {
	s := newCloneSched("", nil)
	s.addListing("A")
	l := s.nextListing()
	got := make(chan string, 1)
	go func() {
		a, _, ok := s.nextDownload()
		if !ok {
			got <- "<none>"
			return
		}
		got <- a.Path
	}()
	select {
	case p := <-got:
		t.Fatalf("download worker did not wait for the listing (got %s)", p)
	case <-time.After(50 * time.Millisecond):
	}
	s.seal(l)
	s.queueDownloads(dl("A/a1"), nil)
	s.listingDone()
	if p := <-got; p != "A/a1" {
		t.Fatalf("got %s, want A/a1", p)
	}
	if _, _, ok := s.nextDownload(); ok {
		t.Fatal("a download handed out after everything finished")
	}

	// stop releases waiting workers.
	s2 := newCloneSched("", nil)
	s2.addListing("A")
	s2.nextListing()
	done := make(chan bool)
	go func() { _, _, ok := s2.nextDownload(); done <- ok }()
	s2.stop()
	if ok := <-done; ok {
		t.Fatal("stopped queue handed out a download")
	}
}

// A choice made after a listing failed, or after every listing finished, is
// never carved: no listing worker is left to run it, and the download workers
// would wait for it for ever (the first sync stuck at "Syncing").
func TestCloneSchedNoCarveWithNobodyToListIt(t *testing.T) {
	s := newCloneSched("", nil)
	s.addListing("A")
	s.addListing("B")
	a := s.nextListing()
	b := s.nextListing()
	if a.rel != "A" || b.rel != "B" {
		t.Fatalf("listings = %s, %s", a.rel, b.rel)
	}
	s.failedListing(a, false) // A's PROPFIND failed
	s.listingDone()
	s.setFirst([]string{"A/Sub"})
	s.mu.Lock()
	n := len(s.pending)
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("a choice inside a failed listing was carved (%d waiting)", n)
	}

	s.seal(b)
	s.listingDone()
	if l := s.nextListing(); l != nil {
		t.Fatalf("listing after all done = %v", l.rel)
	}
	// Listing is over; B was never planned with a carve, but a choice now
	// still must not queue one.
	b.sealed = false
	s.setFirst([]string{"B/Sub"})
	done := make(chan bool, 1)
	go func() { _, _, ok := s.nextDownload(); done <- ok }()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("a download handed out from an empty queue")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("download worker waits for a listing nobody will run")
	}
}

// A carved folder that 404s is only lost if the listing around it saw it: its
// contents were left to the carve, so nobody fetched them and the clone must
// not count as done. A 404 for a folder the server doesn't have is fine.
func TestCloneSchedLostCarves(t *testing.T) {
	s := newCloneSched("", nil)
	s.addListing("Big")
	s.setFirst([]string{"Big/Sub", "Big/Gone"})
	var sub, gone *cloneListing
	for i := 0; i < 3; i++ {
		l := s.nextListing()
		switch l.rel {
		case "Big/Sub":
			sub = l
		case "Big/Gone":
			gone = l
		}
	}
	s.failedListing(sub, true)
	s.failedListing(gone, true)
	s.sawRow("Big/Sub") // Big listed Sub; Gone wasn't there
	if got := s.lostCarves(); !reflect.DeepEqual(got, []string{"Big/Sub"}) {
		t.Fatalf("lostCarves = %v, want [Big/Sub]", got)
	}
}

// cloneTree is a remote account for the order tests: no files at the root, so
// nothing is queued before the listings start.
func cloneTree(files ...string) map[string]davNode {
	nodes := map[string]davNode{"": {isDir: true, etag: "e-root"}}
	for _, f := range files {
		for d := davParent(f); d != ""; d = davParent(d) {
			nodes[d] = davNode{isDir: true, etag: "e-" + d}
		}
		nodes[f] = davNode{etag: "e-" + f, body: "body of " + f}
	}
	return nodes
}

// A folder chosen before the first sync starts, deep inside a big top-level
// folder, downloads before everything else, without waiting for the listing
// of the folder around it, and without anything downloading twice. The
// choice is cleared once the clone is done.
func TestCloneDownloadsChosenFolderFirst(t *testing.T) {
	f := newFakeDAV(cloneTree("A/a1", "A/a2", "Big/x/x1", "Big/Sub/s1", "Big/Sub/deep/s2", "Big/y1", "C/c1"))
	srv := httptest.NewServer(f)
	defer srv.Close()
	e, st := newHookEngine(t, srv.URL)
	e.cloneTestPools = [2]int{1, 1} // one lister, one download: a fixed order
	local := t.TempDir()

	if err := e.SetCloneFirst(local, "", []string{"Big/Sub"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.SyncOnce(context.Background(), Pair{LocalDir: local}); err != nil {
		t.Fatal(err)
	}

	got := f.gets()
	if len(got) != 7 {
		t.Fatalf("GETs = %v, want each of the 7 files once", got)
	}
	if want := []string{"Big/Sub/s1", "Big/Sub/deep/s2"}; !reflect.DeepEqual(got[:2], want) {
		t.Fatalf("first downloads = %v, want %v (all: %v)", got[:2], want, got)
	}
	if f.pfCount("Big/Sub") != 1 {
		t.Errorf("the chosen folder was listed %d times, want once on its own", f.pfCount("Big/Sub"))
	}
	for _, p := range []string{"A/a1", "A/a2", "Big/x/x1", "Big/Sub/s1", "Big/Sub/deep/s2", "Big/y1", "C/c1"} {
		if b, err := os.ReadFile(filepath.Join(local, filepath.FromSlash(p))); err != nil || string(b) != "body of "+p {
			t.Errorf("%s: %q, %v", p, b, err)
		}
	}
	pk := PairKey(local, "")
	base, _ := st.LoadBaseline(pk)
	for _, p := range []string{"Big", "Big/Sub", "Big/Sub/deep", "Big/Sub/s1", "Big/Sub/deep/s2"} {
		if _, ok := base[p]; !ok {
			t.Errorf("no baseline for %s", p)
		}
	}
	if status, _ := st.CloneStatus(pk); status != "done" {
		t.Fatalf("clone status = %q, want done", status)
	}
	if first, _ := st.CloneFirst(pk); first != nil {
		t.Errorf("choice left after the clone finished: %v", first)
	}
	if err := e.SetCloneFirst(local, "", []string{"A"}); !errors.Is(err, errCloneFinished) {
		t.Errorf("choice after the clone finished: err = %v, want errCloneFinished", err)
	}
}

// Choosing a folder while the clone runs moves its files ahead of everything
// still waiting; the file already downloading finishes first.
func TestCloneReprioritisesWhileRunning(t *testing.T) {
	f := newFakeDAV(cloneTree("A/a1", "A/a2", "A/a3", "B/b1", "C/c1", "C/c2"))
	held, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.onGET = func(rel string) {
		if rel == "A/a1" {
			once.Do(func() { close(held) })
			<-release
		}
	}
	srv := httptest.NewServer(f)
	defer srv.Close()
	e, _ := newHookEngine(t, srv.URL)
	e.cloneTestPools = [2]int{1, 1}
	local := t.TempDir()

	errc := make(chan error, 1)
	go func() {
		_, err := e.SyncOnce(context.Background(), Pair{LocalDir: local})
		errc <- err
	}()
	<-held
	// Wait until every folder is listed, so the queue holds all of them.
	waitFor(t, "every folder listed", func() bool {
		p := e.Progress()
		return p.Active && !p.Enumerating && len(p.Clones) == 1
	})
	if run := e.Progress().Clones[0]; run.LocalDir != local || len(run.First) != 0 {
		t.Fatalf("clone run = %+v", run)
	}
	if err := e.SetCloneFirst(local, "", []string{"C"}); err != nil {
		t.Fatal(err)
	}
	if run := e.Progress().Clones[0]; !reflect.DeepEqual(run.First, []string{"C"}) {
		t.Fatalf("clone run after the choice = %+v", run)
	}
	close(release)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	want := []string{"A/a1", "C/c1", "C/c2", "A/a2", "A/a3", "B/b1"}
	if got := f.gets(); !reflect.DeepEqual(got, want) {
		t.Fatalf("download order = %v\nwant %v", got, want)
	}
	if runs := e.Progress().Clones; len(runs) != 0 {
		t.Fatalf("clone still listed after it finished: %+v", runs)
	}
}

// A clone interrupted mid-way keeps its choice for the resume, which puts the
// chosen folder first again (skipping what is already down).
func TestCloneChoiceSurvivesInterruption(t *testing.T) {
	f := newFakeDAV(cloneTree("A/a1", "A/a2", "B/b1", "C/c1"))
	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	f.onGET = func(rel string) {
		once.Do(cancel) // the first download: stop the clone there
	}
	f.setFailGET("C/c1", 500) // and that download does not land
	srv := httptest.NewServer(f)
	defer srv.Close()
	e, st := newHookEngine(t, srv.URL)
	e.cloneTestPools = [2]int{1, 1}
	local := t.TempDir()
	pk := PairKey(local, "")

	if err := e.SetCloneFirst(local, "", []string{"C"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.SyncOnce(ctx, Pair{LocalDir: local}); err == nil {
		t.Fatal("interrupted clone reported success")
	}
	if status, _ := st.CloneStatus(pk); status != "started" {
		t.Fatalf("status after interruption = %q, want started", status)
	}
	if first, _ := st.CloneFirst(pk); !reflect.DeepEqual(first, []string{"C"}) {
		t.Fatalf("choice after interruption = %v, want [C]", first)
	}

	before := f.gets()
	if len(before) == 0 || before[0] != "C/c1" {
		t.Fatalf("interrupted run's downloads = %v, want C/c1 first", before)
	}
	f.clearFailGET("C/c1")

	if _, err := e.SyncOnce(context.Background(), Pair{LocalDir: local}); err != nil {
		t.Fatal(err)
	}
	resumed := f.gets()[len(before):]
	want := []string{"C/c1", "A/a1", "A/a2", "B/b1"}
	if !reflect.DeepEqual(resumed, want) {
		t.Fatalf("resumed downloads = %v, want %v", resumed, want)
	}
	if status, _ := st.CloneStatus(pk); status != "done" {
		t.Fatalf("status after resume = %q, want done", status)
	}
	if first, _ := st.CloneFirst(pk); first != nil {
		t.Errorf("choice left after the clone finished: %v", first)
	}
}

// A download that fails during the first sync must be tried again. Later
// passes dirty the folders above a failed path so the next scan lists them
// again (maintainDirBaselines); the first sync never did, so its folder rows
// kept the etag the listing gave them, the folder looked unchanged from then
// on, and the file it was missing was never fetched: five files on the test VM
// after the server answered 423 to their downloads, with the app saying "Up to
// date" (2026-10-13).
func TestCloneRetriesADownloadThatFailed(t *testing.T) {
	f := newFakeDAV(cloneTree("A/a1", "A/a2", "A/deep/d1", "B/b1"))
	srv := httptest.NewServer(f)
	defer srv.Close()
	e, _ := newHookEngine(t, srv.URL)
	local := t.TempDir()
	pair := Pair{LocalDir: local, RemoteRoot: ""}
	ctx := context.Background()

	shortLockedGETWaits(t)         // the 423 outlasts the download's own tries (Deck #950)
	f.setFailGET("A/deep/d1", 423) // locked on the server while the first sync runs
	if _, err := e.SyncOnce(ctx, pair); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(local, "A", "a1")); err != nil {
		t.Fatalf("the rest of the first sync did not arrive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(local, "A", "deep", "d1")); !os.IsNotExist(err) {
		t.Fatalf("the failing download is here already (err %v)", err)
	}

	f.clearFailGET("A/deep/d1") // the lock is gone; nothing else changed on the server
	if _, err := e.syncRemoteDelta(ctx, pair); err != nil {
		t.Fatalf("the pass after the first sync: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(local, "A", "deep", "d1")); err != nil || string(b) != "body of A/deep/d1" {
		t.Fatalf("the file whose download failed was not tried again: %q, %v", b, err)
	}
}

// shortLockedGETWaits makes the waits between tries of a download the server
// answered 423 to too short to slow a test.
func shortLockedGETWaits(t *testing.T) {
	t.Helper()
	old := transport.LockedGETWaits
	transport.LockedGETWaits = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { transport.LockedGETWaits = old })
}

// A 423 to a download is the server's own lock on the file for a moment, and
// the download is asked for again a moment later. So the five files the test
// VM's first sync left behind (above) now arrive in the first sync itself, with
// no failure to show for them (Deck #950).
func TestCloneDownloadsAFileTheServerHadLockedForAMoment(t *testing.T) {
	shortLockedGETWaits(t)
	f := newFakeDAV(cloneTree("A/a1", "A/deep/d1", "B/b1"))
	srv := httptest.NewServer(f)
	defer srv.Close()
	e, _ := newHookEngine(t, srv.URL)
	local := t.TempDir()

	f.setFailGET("A/deep/d1", 423)
	var mu sync.Mutex
	asked := 0
	f.onGET = func(rel string) {
		if rel != "A/deep/d1" {
			return
		}
		mu.Lock()
		asked++
		third := asked == 3
		mu.Unlock()
		if third {
			f.clearFailGET("A/deep/d1") // the server lets go before the third try
		}
	}
	if _, err := e.SyncOnce(context.Background(), Pair{LocalDir: local, RemoteRoot: ""}); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(local, "A", "deep", "d1")); err != nil || string(b) != "body of A/deep/d1" {
		t.Fatalf("the briefly locked file did not arrive in the first sync: %q, %v", b, err)
	}
	if n := f.getCount("A/deep/d1"); n != 3 {
		t.Errorf("the file was asked for %d times, want 3", n)
	}
	for _, ev := range e.recorder.Errors() {
		t.Errorf("activity shows a failure for %s: %s", ev.Path, ev.Err)
	}
}
