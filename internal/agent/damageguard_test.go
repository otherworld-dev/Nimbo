package agent

// The damage guard, driven through real sync passes against the fakeDAV.
//
// These tests protect the guard's four load-bearing rules on ORDINARY two-way
// folders — where it now runs for everyone:
//
//  1. a pass that would delete or replace most of a folder is refused whole;
//  2. a pass of pure additions is never refused, however large;
//  3. the freeze survives until a human resumes it, and the resume covers
//     exactly one pass;
//  4. an unreadable guard state suspends the account rather than running
//     unguarded.

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/otherworld/nimbo/internal/config"
)

// seededPair builds a synced pair with n files whose baseline is settled, so
// the guard is armed (GuardApplies needs clone status "done").
func seededPair(t *testing.T, n int) (*fakeDAV, *Engine, Pair) {
	t.Helper()
	nodes := map[string]davNode{"": {isDir: true, etag: "e-root"}}
	for i := 0; i < n; i++ {
		nodes[fmt.Sprintf("f%03d.txt", i)] = davNode{etag: fmt.Sprintf("e%03d", i), body: "v1"}
	}
	f := newFakeDAV(nodes)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	e, _ := newHookEngine(t, srv.URL)

	p := Pair{LocalDir: t.TempDir()}
	if _, err := e.SyncOnce(context.Background(), p); err != nil {
		t.Fatalf("seed sync: %v", err)
	}
	// Settle the clone into "done" with a second, quiet pass.
	if _, err := e.SyncOnce(context.Background(), p); err != nil {
		t.Fatalf("settle sync: %v", err)
	}
	return f, e, p
}

// Rule 1, the deletion shape: the server loses most of the folder.
func TestGuardFreezesAMassServerDeletion(t *testing.T) {
	f, e, p := seededPair(t, 60)
	for i := 0; i < 60; i++ {
		f.delNode(fmt.Sprintf("f%03d.txt", i)) // gone from the listing
	}
	f.setNode("", davNode{isDir: true, etag: "e-root2"})

	_, err := e.SyncOnce(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("a mass server deletion must pause the folder, got err=%v", err)
	}
	// The freeze refused the WHOLE pass: every local file must survive.
	for i := 0; i < 60; i++ {
		if _, err := os.Stat(filepath.Join(p.LocalDir, fmt.Sprintf("f%03d.txt", i))); err != nil {
			t.Fatalf("local file %d was touched by a refused pass: %v", i, err)
		}
	}
}

// Rule 1, the ransomware shape: nothing deleted, everything REPLACED. A
// deletions-only guard passes this; ours must not.
func TestGuardFreezesAMassServerRewrite(t *testing.T) {
	f, e, p := seededPair(t, 60)
	for i := 0; i < 60; i++ {
		name := fmt.Sprintf("f%03d.txt", i)
		f.setNode(name, davNode{etag: fmt.Sprintf("x%03d", i), body: "ENCRYPTED"})
	}
	f.setNode("", davNode{isDir: true, etag: "e-root2"})

	_, err := e.SyncOnce(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("a mass rewrite must pause the folder, got err=%v", err)
	}
	// Nothing downloaded: the local copies still hold the original content.
	b, rerr := os.ReadFile(filepath.Join(p.LocalDir, "f000.txt"))
	if rerr != nil || string(b) != "v1" {
		t.Fatalf("refused pass changed a local file: %q err=%v", b, rerr)
	}
}

// Rule 2: a bulk IMPORT is additions, and must never trip the guard.
func TestGuardIgnoresAMassAddition(t *testing.T) {
	f, e, p := seededPair(t, 60)
	for i := 0; i < 200; i++ {
		f.setNode(fmt.Sprintf("new%03d.txt", i), davNode{etag: fmt.Sprintf("n%03d", i), body: "new"})
	}
	f.setNode("", davNode{isDir: true, etag: "e-root2"})

	if _, err := e.SyncOnce(context.Background(), p); err != nil {
		t.Fatalf("a mass addition must sync normally, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(p.LocalDir, "new199.txt")); err != nil {
		t.Fatalf("the additions were not downloaded: %v", err)
	}
}

// Rule 3: frozen stays frozen across passes; a resume covers exactly one pass,
// and the pass it covers applies the damage the user approved.
func TestGuardFreezeHoldsUntilResumedAndResumeCoversOnePass(t *testing.T) {
	f, e, p := seededPair(t, 60)
	for i := 0; i < 60; i++ {
		f.delNode(fmt.Sprintf("f%03d.txt", i))
	}
	f.setNode("", davNode{isDir: true, etag: "e-root2"})

	if _, err := e.SyncOnce(context.Background(), p); err == nil {
		t.Fatal("first pass must freeze")
	}
	// Still frozen on the next pass — the freeze is persistent, not per-pass.
	if _, err := e.SyncOnce(context.Background(), p); err == nil ||
		!strings.Contains(err.Error(), "paused") {
		t.Fatalf("second pass must still be refused, got %v", err)
	}

	// ClearFreeze resolves the folder through the configured pair list.
	if err := e.dirs.SavePairs([]config.SyncPair{{LocalDir: p.LocalDir, RemoteRoot: ""}}); err != nil {
		t.Fatal(err)
	}
	if err := e.ClearFreeze(p.LocalDir); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if _, err := e.SyncOnce(context.Background(), p); err != nil {
		t.Fatalf("the pass the user approved must run, got %v", err)
	}
	// The approved pass applied the deletions.
	if _, err := os.Stat(filepath.Join(p.LocalDir, "f000.txt")); !os.IsNotExist(err) {
		t.Fatal("the approved pass should have applied the deletion")
	}
}

// Rule 3, the other half: resuming a folder that is not frozen is refused —
// this is the call a stale UI button makes.
func TestResumingAnUnfrozenFolderIsRefused(t *testing.T) {
	_, e, p := seededPair(t, 3)
	if err := e.dirs.SavePairs([]config.SyncPair{{LocalDir: p.LocalDir, RemoteRoot: ""}}); err != nil {
		t.Fatal(err)
	}
	if err := e.ClearFreeze(p.LocalDir); err == nil {
		t.Fatal("resuming an unfrozen folder must be refused")
	}
}

// Rule 4: an unreadable guard state suspends the account. Presence in that file
// is what records a freeze, so "unreadable" must not read as "nothing frozen".
func TestUnreadableGuardStateSuspendsSyncing(t *testing.T) {
	_, e, p := seededPair(t, 3)
	if err := os.WriteFile(e.dirs.GuardStateFile(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := e.SyncOnce(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "suspended") {
		t.Fatalf("an unreadable guard state must suspend the pass, got %v", err)
	}
}

// The freeze survives a state-database reset: it lives in the guard file, and
// ensurePair checks it upstream of the clone a reset would otherwise trigger.
func TestFreezeSurvivesAStateReset(t *testing.T) {
	f, e, p := seededPair(t, 60)
	for i := 0; i < 60; i++ {
		f.delNode(fmt.Sprintf("f%03d.txt", i))
	}
	f.setNode("", davNode{isDir: true, etag: "e-root2"})
	if _, err := e.SyncOnce(context.Background(), p); err == nil {
		t.Fatal("must freeze first")
	}

	// Simulate the reset: wipe the state DB the way a support "clear sync
	// state" would, leaving the guard file alone.
	e.closeStore()
	db := e.dirs.StateDB(e.dirs.AccountID())
	for _, f := range []string{db, db + "-wal", db + "-shm"} {
		_ = os.Remove(f)
	}

	if _, err := e.SyncOnce(context.Background(), p); err == nil ||
		!strings.Contains(err.Error(), "paused") {
		t.Fatalf("a state reset must not unfreeze the folder, got %v", err)
	}
	// And nothing was re-cloned over the local copy.
	b, rerr := os.ReadFile(filepath.Join(p.LocalDir, "f000.txt"))
	if rerr != nil || string(b) != "v1" {
		t.Fatalf("the frozen folder was touched after a reset: %q err=%v", b, rerr)
	}
}

// Removing a folder clears its guard entry — via ForgetSyncFolder — so a
// re-added folder does not inherit a stale freeze.
func TestForgetSyncFolderDropsTheGuardEntry(t *testing.T) {
	f, e, p := seededPair(t, 60)
	for i := 0; i < 60; i++ {
		f.delNode(fmt.Sprintf("f%03d.txt", i))
	}
	f.setNode("", davNode{isDir: true, etag: "e-root2"})
	if _, err := e.SyncOnce(context.Background(), p); err == nil {
		t.Fatal("must freeze first")
	}

	// Register the pair so ForgetSyncFolder can find and remove it.
	if err := e.dirs.SavePairs([]config.SyncPair{{LocalDir: p.LocalDir, RemoteRoot: ""}}); err != nil {
		t.Fatal(err)
	}
	if err := e.ForgetSyncFolder("", false); err != nil {
		t.Fatalf("forget: %v", err)
	}
	set, err := e.dirs.LoadGuardState()
	if err != nil {
		t.Fatal(err)
	}
	if len(set) != 0 {
		t.Fatalf("the guard entry must go with the folder, still holds %v", set)
	}
}
