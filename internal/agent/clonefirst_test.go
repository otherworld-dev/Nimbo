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
