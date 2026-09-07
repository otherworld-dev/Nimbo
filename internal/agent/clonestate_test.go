package agent

import (
	"path/filepath"
	"testing"

	"github.com/otherworld/nimbo/internal/config"
)

// Removing a sync folder must forget its initial-clone state along with its
// checkpoint. The row is keyed by (local dir, remote root), so re-adding the
// same folder later finds it again — and a leftover "started" turns that
// re-add into a clone RESUME, where decideCloneFile refetches (overwrites) any
// local file whose size differs from the server. The takeover path a fresh
// pair gets never overwrites. GitHub #4 left exactly this row behind for a
// user's official-client folder.
func TestRemoveSyncFolderClearsCloneStatus(t *testing.T) {
	e, st := newHookEngine(t, "http://unused.invalid") // no client traffic on this path
	local := t.TempDir()
	if err := e.dirs.SavePairs([]config.SyncPair{{LocalDir: local, RemoteRoot: "Photos"}}); err != nil {
		t.Fatal(err)
	}
	pk := PairKey(local, "Photos")
	if err := st.SetCloneStatus(pk, "started"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetCloneStatus("decoy-pair", "done"); err != nil {
		t.Fatal(err)
	}

	if err := e.RemoveSyncFolder("Photos", false); err != nil {
		t.Fatal(err)
	}

	if got, _ := st.CloneStatus(pk); got != "" {
		t.Fatalf("clone status after remove = %q, want %q (a re-added folder would resume, not take over)", got, "")
	}
	if got, _ := st.CloneStatus("decoy-pair"); got != "done" {
		t.Fatalf("unrelated pair's clone status = %q, want %q", got, "done")
	}
}

// Adding a folder must discard any clone-state row already sitting under its
// key. Rows written by builds before the clears above outlive their pair, so a
// user who removed a folder on an old build and re-adds it on a new one would
// otherwise RESUME into a populated folder (overwrite) rather than take it
// over. Resume is only ever right for an interrupted clone of a pair that is
// STILL configured — a folder the user adds is a new pair from their point of
// view. (GitHub #4: the reporter's official-client folder carries such a row.)
func TestAddSyncFolderDiscardsStaleCloneStatus(t *testing.T) {
	e, st := newHookEngine(t, "http://unused.invalid")
	base := t.TempDir()
	if err := e.SetBaseDir(base); err != nil {
		t.Fatal(err)
	}
	pk := PairKey(filepath.Join(base, "Photos"), "Photos")
	if err := st.SetCloneStatus(pk, "started"); err != nil {
		t.Fatal(err)
	}

	if err := e.AddSyncFolder("Photos"); err != nil {
		t.Fatal(err)
	}

	if got, _ := st.CloneStatus(pk); got != "" {
		t.Fatalf("clone status after add = %q, want %q (a stale row would resume, not take over)", got, "")
	}
}

// Same for the explicit-local-dir form the GUI's setup flow uses.
func TestAddSyncPairDiscardsStaleCloneStatus(t *testing.T) {
	e, st := newHookEngine(t, "http://unused.invalid")
	local := filepath.Join(t.TempDir(), "Nimbo")
	pk := PairKey(local, "")
	if err := st.SetCloneStatus(pk, "started"); err != nil {
		t.Fatal(err)
	}

	if err := e.AddSyncPair(local, ""); err != nil {
		t.Fatal(err)
	}

	if got, _ := st.CloneStatus(pk); got != "" {
		t.Fatalf("clone status after add = %q, want %q", got, "")
	}
}

// The replay case must keep working: signing back into an account whose
// config survived re-runs the setup flow against a pair that ALREADY exists.
// That pair's clone may be mid-flight, and its row is what lets it resume —
// the discard applies to new pairs only.
func TestAddSyncPairKeepsCloneStatusOfExistingPair(t *testing.T) {
	e, st := newHookEngine(t, "http://unused.invalid")
	local := filepath.Join(t.TempDir(), "Nimbo")
	if err := e.dirs.SavePairs([]config.SyncPair{{LocalDir: local, RemoteRoot: ""}}); err != nil {
		t.Fatal(err)
	}
	pk := PairKey(local, "")
	if err := st.SetCloneStatus(pk, "started"); err != nil {
		t.Fatal(err)
	}

	if err := e.AddSyncPair(local, ""); err != nil {
		t.Fatal(err)
	}

	if got, _ := st.CloneStatus(pk); got != "started" {
		t.Fatalf("clone status of an already-configured pair = %q, want %q (an interrupted clone must still resume)", got, "started")
	}
}

// ResetPairState promises the next sync "treats the pair as brand new". With
// the clone row left behind that is untrue in the one way that matters: a
// populated folder would be resumed into (overwrite) rather than taken over.
func TestResetPairStateClearsCloneStatus(t *testing.T) {
	e, st := newHookEngine(t, "http://unused.invalid")
	local := t.TempDir()
	pk := PairKey(local, "Photos")
	if err := st.SetCloneStatus(pk, "started"); err != nil {
		t.Fatal(err)
	}

	if err := e.ResetPairState(local, "Photos"); err != nil {
		t.Fatal(err)
	}

	if got, _ := st.CloneStatus(pk); got != "" {
		t.Fatalf("clone status after reset = %q, want %q", got, "")
	}
}
