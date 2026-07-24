package agent

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestMoveSyncExclusion is the core safety invariant of the redesigned "Move sync
// folder": a folder move and a sync pass can NEVER run at the same time. This is
// what makes the original data-loss bug — a folder relocated while a sync was
// live, which the sync then read as mass server deletions — structurally
// impossible rather than something merely guarded against after the fact.
func TestMoveSyncExclusion(t *testing.T) {
	e := &Engine{}

	// A sync pass is running → a move must be refused (so no relocate can start).
	if !e.beginSyncPass() {
		t.Fatal("beginSyncPass should succeed when idle")
	}
	if e.beginMove() {
		e.endMove()
		t.Fatal("beginMove must FAIL while a sync pass is running")
	}
	e.endSyncPass()

	// Sync finished → a move can now take exclusive control.
	if !e.beginMove() {
		t.Fatal("beginMove should succeed once no sync is running")
	}
	// A move is in progress → sync passes must skip, not run.
	if e.beginSyncPass() {
		e.endSyncPass()
		t.Fatal("beginSyncPass must FAIL (skip) while a move is in progress")
	}
	e.endMove()

	// Move done → syncs resume.
	if !e.beginSyncPass() {
		t.Fatal("beginSyncPass should succeed after the move ends")
	}
	e.endSyncPass()

	// Multiple sync passes may run concurrently (read lock), and a move stays
	// blocked until every one of them has finished — covers the case where one
	// of several concurrent pairs is still mid-sync when a move is attempted.
	if !e.beginSyncPass() || !e.beginSyncPass() {
		t.Fatal("two concurrent sync passes should both acquire the read lock")
	}
	if e.beginMove() {
		e.endMove()
		t.Fatal("beginMove must fail while two sync passes are running")
	}
	e.endSyncPass()
	if e.beginMove() {
		e.endMove()
		t.Fatal("beginMove must still fail with one sync pass remaining")
	}
	e.endSyncPass()
	if !e.beginMove() {
		t.Fatal("beginMove should succeed once the last sync pass ends")
	}
	e.endMove()
}

// TestMoveSyncPairRefusesDuringSync proves the move-specific safety property: with
// a sync pass in flight, MoveSyncPair refuses up front and performs NO relocate —
// the exact thing that, in the original bug, let a move delete server files. It
// also checks the refusal doesn't leak the move lock.
func TestMoveSyncPairRefusesDuringSync(t *testing.T) {
	e := &Engine{}

	// Simulate a sync pass in flight.
	if !e.beginSyncPass() {
		t.Fatal("setup: beginSyncPass should succeed")
	}

	relocated := false
	newLocal := filepath.Join(t.TempDir(), "dest")
	err := e.MoveSyncPair(filepath.Join(t.TempDir(), "old"), newLocal, func() error {
		relocated = true
		return nil
	})

	if err == nil {
		t.Fatal("MoveSyncPair must refuse while a sync is in flight")
	}
	if !strings.Contains(err.Error(), "sync is in progress") {
		t.Errorf("expected a 'sync in progress' error, got: %v", err)
	}
	if relocated {
		t.Fatal("relocate must NOT run when the move is refused — no files may move")
	}

	// The refusal must not leak the move lock: once the sync ends, a move can proceed.
	e.endSyncPass()
	if !e.beginMove() {
		t.Fatal("move lock leaked: beginMove should succeed after a refused move + sync end")
	}
	e.endMove()
}
