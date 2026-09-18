package agent

// The post-revert window (Deck #571, #678), driven through real passes against
// the fakeDAV: the first live pass after leaving virtual-files mode restores
// what the revert could not materialise rather than deleting or conflicting
// it, and normal semantics return once that pass is clean.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/otherworld/nimbo/internal/transfer"
)

// stubGoneServerChanged sets up what the test VM showed on 2026-09-14: a file
// synced in a live era, then (while the account was in virtual-files mode)
// edited on the server and never opened locally, so the revert deleted its
// stub. Locally absent, server etag differs from the baseline.
func stubGoneServerChanged(t *testing.T) (*fakeDAV, *Engine, Pair) {
	t.Helper()
	f, e, p := seededPair(t, 3)
	// The GUI's default: conflicts wait for the user. The test engine's zero
	// policy would quietly keep-both a deleted-locally conflict by
	// re-downloading, which is exactly what hid this on every machine but the
	// VM (whose Settings asked).
	e.SetConflictPolicy(transfer.PolicyAsk)
	f.setNode("f001.txt", davNode{etag: "x001", body: "v2 edited on the server"})
	f.setNode("", davNode{isDir: true, etag: "e-root2"})
	if err := os.Remove(filepath.Join(p.LocalDir, "f001.txt")); err != nil {
		t.Fatal(err)
	}
	return f, e, p
}

func TestPostRevertRedownloadsAStubWhoseServerCopyChanged(t *testing.T) {
	f, e, p := stubGoneServerChanged(t)
	if err := e.dirs.MarkPostRevert(p.LocalDir); err != nil {
		t.Fatal(err)
	}
	if _, err := e.SyncOnce(context.Background(), p); err != nil {
		t.Fatalf("first live pass: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(p.LocalDir, "f001.txt"))
	if err != nil || string(b) != "v2 edited on the server" {
		t.Errorf("file not re-downloaded: %q, %v", b, err)
	}
	if n := len(e.PendingConflicts()); n != 0 {
		t.Errorf("%d conflict(s) pending for a file nobody touched locally: %+v", n, e.PendingConflicts())
	}
	f.mu.Lock()
	deletes := append([]string(nil), f.deletes...)
	f.mu.Unlock()
	if len(deletes) != 0 {
		t.Errorf("server touched: DELETE %v", deletes)
	}
	// A clean first pass closes the window; ordinary delete semantics are back.
	if e.dirs.IsPostRevert(p.LocalDir) {
		t.Error("post-revert window still open after a clean pass")
	}
}

// Outside the window the same situation IS a conflict — the user deleted a
// file the server changed — and must stay one.
func TestOutsidePostRevertWindowItStaysAConflict(t *testing.T) {
	_, e, p := stubGoneServerChanged(t)
	if _, err := e.SyncOnce(context.Background(), p); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if n := len(e.PendingConflicts()); n != 1 {
		t.Fatalf("want exactly one pending conflict, got %d: %+v", n, e.PendingConflicts())
	}
	if _, err := os.Stat(filepath.Join(p.LocalDir, "f001.txt")); err == nil {
		t.Error("a genuine deleted-locally conflict was auto-resolved by a download")
	}
}
