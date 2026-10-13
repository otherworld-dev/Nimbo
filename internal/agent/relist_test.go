package agent

// The one-off re-list that finds files an older build's first sync left out
// (Deck #949), driven through real passes against the fakeDAV.

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/otherworld/nimbo/internal/transfer"
)

type toastLog struct {
	mu     sync.Mutex
	titles []string
	bodies []string
}

func (l *toastLog) add(title, body, _ string) {
	l.mu.Lock()
	l.titles = append(l.titles, title)
	l.bodies = append(l.bodies, body)
	l.mu.Unlock()
}

func (l *toastLog) count(title string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, t := range l.titles {
		if t == title {
			n++
		}
	}
	return n
}

func (l *toastLog) body(title string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, t := range l.titles {
		if t == title {
			return l.bodies[i]
		}
	}
	return ""
}

// A pair first synced by an older build has a file the server holds and this
// PC does not, in a folder whose etag has not changed since: the scan prunes
// the folder and never sees it. The one-off re-list lists every folder once,
// fetches the file, tells the user why and what it found, and does not run
// again.
func TestRelistOnceFindsAFileTheFirstSyncLeftOut(t *testing.T) {
	f := newFakeDAV(cloneTree("A/a1", "A/a2", "A/deep/d1", "B/b1"))
	srv := httptest.NewServer(f)
	defer srv.Close()
	e, st := newHookEngine(t, srv.URL)
	var toasts toastLog
	e.SetToastFunc(toasts.add)
	local := t.TempDir()
	pair := Pair{LocalDir: local, RemoteRoot: ""}
	pk := PairKey(local, "")
	ctx := context.Background()
	if _, err := e.SyncOnce(ctx, pair); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	// A first sync by THIS build has nothing to find, and says nothing.
	if v, _ := st.PairMeta(pk, relistMeta); v != relistDone {
		t.Fatalf("after a first sync the re-list is %q, want done", v)
	}
	if n := toasts.count(relistStartTitle); n != 0 {
		t.Fatalf("a fresh first sync announced a folder check (%d toasts)", n)
	}

	// The hole an older build left: on the server, not here, folder etags
	// unchanged. No pass finds it.
	f.setNode("A/deep/missed", davNode{etag: "e-missed", body: "the file left out"})
	missed := filepath.Join(local, "A", "deep", "missed")
	if _, err := e.syncRemoteDelta(ctx, pair); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(missed); !os.IsNotExist(err) {
		t.Fatalf("the hole healed without the re-list (err %v); the test no longer shows the problem", err)
	}

	// The same pair as an older build left it: never re-listed.
	if err := st.SetPairMeta(pk, relistMeta, ""); err != nil {
		t.Fatal(err)
	}
	listedBefore := f.pfCount("B")
	if _, err := e.syncRemoteDelta(ctx, pair); err != nil {
		t.Fatalf("the re-list pass: %v", err)
	}
	if b, err := os.ReadFile(missed); err != nil || string(b) != "the file left out" {
		t.Fatalf("the re-list did not fetch the missing file: %q, %v", b, err)
	}
	if f.pfCount("B") == listedBefore {
		t.Error("an unchanged folder was not listed again: the re-list must list every folder")
	}
	if v, _ := st.PairMeta(pk, relistMeta); v != relistDone {
		t.Errorf("after the re-list its state is %q, want done", v)
	}
	if toasts.count(relistStartTitle) != 1 || toasts.count(relistDoneTitle) != 1 {
		t.Fatalf("toasts = %v, want one start and one finish", toasts.titles)
	}
	if b := toasts.body(relistStartTitle); !strings.Contains(b, "earlier version") || !strings.Contains(b, filepath.Base(local)) {
		t.Errorf("the start toast should say why and which folder: %q", b)
	}
	if b := toasts.body(relistDoneTitle); !strings.Contains(b, "1 file") {
		t.Errorf("the finish toast should say one file was fetched: %q", b)
	}

	// Once only: later passes prune again and say nothing.
	listedAfter := f.pfCount("B")
	if _, err := e.syncRemoteDelta(ctx, pair); err != nil {
		t.Fatal(err)
	}
	if f.pfCount("B") != listedAfter {
		t.Error("the folders were listed again on the next pass: the re-list must run once")
	}
	if toasts.count(relistStartTitle) != 1 {
		t.Error("the folder check was announced a second time")
	}
}

// One sync folder at a time, so several folders never crawl the server
// together; a pass that fails leaves the re-list open for the next one; and
// nothing was missing reads as such.
func TestRelistRunsOneFolderAtATime(t *testing.T) {
	f := newFakeDAV(cloneTree("A/a1", "B/b1"))
	srv := httptest.NewServer(f)
	defer srv.Close()
	e, st := newHookEngine(t, srv.URL)
	var toasts toastLog
	e.SetToastFunc(toasts.add)
	one := Pair{LocalDir: filepath.Join(t.TempDir(), "One"), RemoteRoot: "A"}
	two := Pair{LocalDir: filepath.Join(t.TempDir(), "Two"), RemoteRoot: "B"}
	pk1, pk2 := PairKey(one.LocalDir, one.RemoteRoot), PairKey(two.LocalDir, two.RemoteRoot)

	if !e.relistBegin(st, pk1, one) {
		t.Fatal("the first folder's check did not start")
	}
	if e.relistBegin(st, pk2, two) {
		t.Fatal("a second folder's check started while the first was running")
	}
	// The first folder's pass fails: still running, still holding the turn.
	e.relistEnd(st, pk1, one, true, transfer.Stats{}, errors.New("scan failed"))
	if v, _ := st.PairMeta(pk1, relistMeta); v != relistRunning {
		t.Fatalf("after a failed pass the state is %q, want running", v)
	}
	if e.relistBegin(st, pk2, two) {
		t.Fatal("the second folder's check started after the first one's pass failed")
	}
	// Its next pass picks it up again without a second announcement...
	if !e.relistBegin(st, pk1, one) {
		t.Fatal("the first folder's check did not carry on")
	}
	if n := toasts.count(relistStartTitle); n != 1 {
		t.Fatalf("%d start toasts for one folder, want 1", n)
	}
	// ...and finishes, which lets the second folder go.
	e.relistEnd(st, pk1, one, true, transfer.Stats{}, nil)
	if b := toasts.body(relistDoneTitle); !strings.Contains(b, "nothing was missing") {
		t.Errorf("finish toast with nothing fetched: %q", b)
	}
	if !e.relistBegin(st, pk2, two) {
		t.Fatal("the second folder's check did not start once the first had finished")
	}
	// A finished folder never starts again.
	if e.relistBegin(st, pk1, one) {
		t.Fatal("a finished check started again")
	}
}
