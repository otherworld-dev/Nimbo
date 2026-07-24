package agent

import (
	"testing"

	"github.com/otherworld/nimbo/internal/engine"
)

// TestBlockedMergeVsReplace locks in the fix for the emptied "Can't sync" menu: a
// full reconcile replaces the blocked list, but a scoped/delta sync must only
// merge its finds in — never clear blocks it didn't re-examine.
func TestBlockedMergeVsReplace(t *testing.T) {
	e := &Engine{blocked: make(map[string][]engine.Blocked)}

	// A full sync finds two un-syncable files.
	e.setBlocked("d", []engine.Blocked{{Path: "x/a"}, {Path: "x/b"}})
	if n := len(e.blocked["d"]); n != 2 {
		t.Fatalf("after full setBlocked: %d, want 2", n)
	}

	// A delta sync that examined unrelated paths (found nothing blocked) must NOT
	// clear the list — this was the bug.
	e.addBlocked("d", nil)
	if n := len(e.blocked["d"]); n != 2 {
		t.Fatalf("delta wiped the blocked list: %d, want 2", n)
	}

	// A delta that finds a new blocked file merges it (deduped).
	e.addBlocked("d", []engine.Blocked{{Path: "y/c"}, {Path: "x/a"}})
	if n := len(e.blocked["d"]); n != 3 {
		t.Fatalf("after addBlocked: %d, want 3", n)
	}

	// A new full sync is authoritative — it replaces.
	e.setBlocked("d", []engine.Blocked{{Path: "x/a"}})
	if n := len(e.blocked["d"]); n != 1 {
		t.Fatalf("full replace: %d, want 1", n)
	}

	// Replacing with empty clears the pair entirely.
	e.setBlocked("d", nil)
	if _, ok := e.blocked["d"]; ok {
		t.Fatal("empty full sync should clear the pair's blocked entry")
	}
}
