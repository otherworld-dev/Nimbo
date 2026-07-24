package state

import (
	"path/filepath"
	"testing"

	"github.com/otherworld/nimbo/internal/engine"
)

func openTemp(t *testing.T, cache bool) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"), "acct1", cache)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestRekeyPairMovesBaselineAndCloneStatus(t *testing.T) {
	for _, cache := range []bool{false, true} {
		s := openTemp(t, cache)
		const oldKey, newKey = "aaaa1111", "bbbb2222"
		for _, p := range []string{"a.txt", "dir/b.txt"} {
			if err := s.UpsertBaseline(oldKey, engine.BaselineState{Path: p, RemoteETag: "e", RemoteFileID: "1"}); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.SetCloneStatus(oldKey, "done"); err != nil {
			t.Fatal(err)
		}
		// Touch the baseline first so the resident-cache path is exercised when on.
		if _, err := s.LoadBaseline(oldKey); err != nil {
			t.Fatal(err)
		}

		if err := s.RekeyPair(oldKey, newKey); err != nil {
			t.Fatalf("cache=%v: rekey: %v", cache, err)
		}

		if m, _ := s.LoadBaseline(oldKey); len(m) != 0 {
			t.Errorf("cache=%v: oldKey still has %d rows", cache, len(m))
		}
		if st, _ := s.CloneStatus(oldKey); st != "" {
			t.Errorf("cache=%v: oldKey clone status = %q, want empty", cache, st)
		}
		if m, _ := s.LoadBaseline(newKey); len(m) != 2 {
			t.Errorf("cache=%v: newKey has %d rows, want 2", cache, len(m))
		}
		if st, _ := s.CloneStatus(newKey); st != "done" {
			t.Errorf("cache=%v: newKey clone status = %q, want done", cache, st)
		}
	}
}

func TestRekeyPairRefusesOccupiedDestination(t *testing.T) {
	s := openTemp(t, true)
	if err := s.UpsertBaseline("oldk", engine.BaselineState{Path: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertBaseline("newk", engine.BaselineState{Path: "y"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RekeyPair("oldk", "newk"); err == nil {
		t.Fatal("expected refusal when destination already has a baseline")
	}
	if m, _ := s.LoadBaseline("oldk"); len(m) != 1 {
		t.Errorf("source baseline disturbed after refusal: %d rows", len(m))
	}
}

func TestRekeyPairSameKeyNoop(t *testing.T) {
	s := openTemp(t, true)
	if err := s.UpsertBaseline("k", engine.BaselineState{Path: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RekeyPair("k", "k"); err != nil {
		t.Fatalf("same-key: %v", err)
	}
	if m, _ := s.LoadBaseline("k"); len(m) != 1 {
		t.Errorf("same-key disturbed baseline: %d rows", len(m))
	}
}
