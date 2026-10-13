package state

import (
	"testing"

	"github.com/otherworld/nimbo/internal/engine"
)

// A pair's named values go with the pair: kept apart from other pairs',
// dropped when the pair is reset and carried over when it moves.
func TestPairMeta(t *testing.T) {
	s := openTemp(t, false)
	if got, err := s.PairMeta("pair-a", "relist-once"); err != nil || got != "" {
		t.Fatalf("nothing set = %q, %v; want empty, nil", got, err)
	}
	if err := s.SetPairMeta("pair-a", "relist-once", "running"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPairMeta("pair-b", "relist-once", "done"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPairMeta("pair-a", "relist-once", "done"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.PairMeta("pair-a", "relist-once"); got != "done" {
		t.Fatalf("after replace = %q, want done", got)
	}
	if got, _ := s.PairMeta("pair-a", "something-else"); got != "" {
		t.Errorf("another name = %q, want empty", got)
	}

	// A move carries it over (RekeyPair needs a baseline to move).
	if err := s.UpsertBaselineBatch("pair-a", []engine.BaselineState{{Path: "f.txt", RemoteETag: "e"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.RekeyPair("pair-a", "pair-moved"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.PairMeta("pair-moved", "relist-once"); got != "done" {
		t.Errorf("after a move = %q, want done", got)
	}
	if got, _ := s.PairMeta("pair-a", "relist-once"); got != "" {
		t.Errorf("the old key still has %q after the move", got)
	}

	// A reset drops it, for that pair only.
	if err := s.ClearCloneStatus("pair-moved"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.PairMeta("pair-moved", "relist-once"); got != "" {
		t.Errorf("after ClearCloneStatus = %q, want empty", got)
	}
	if got, _ := s.PairMeta("pair-b", "relist-once"); got != "done" {
		t.Errorf("unrelated pair = %q, want done", got)
	}
}

// BlankDirETags empties folder etags only: files keep theirs, other pairs are
// untouched, and a store that keeps the baseline in memory sees the change.
func TestBlankDirETags(t *testing.T) {
	for _, cached := range []bool{false, true} {
		s := openTemp(t, cached)
		rows := []engine.BaselineState{
			{Path: "A", IsDir: true, RemoteETag: "e-a", RemoteFileID: "1"},
			{Path: "A/deep", IsDir: true, RemoteETag: "e-deep", RemoteFileID: "2", MountRoot: true},
			{Path: "A/blank", IsDir: true, RemoteETag: ""},
			{Path: "A/file.txt", RemoteETag: "e-file", LocalSize: 3},
		}
		if err := s.UpsertBaselineBatch("pair-a", rows); err != nil {
			t.Fatal(err)
		}
		if err := s.UpsertBaselineBatch("pair-b", []engine.BaselineState{{Path: "B", IsDir: true, RemoteETag: "e-b"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.LoadBaseline("pair-a"); err != nil { // warm the resident copy, when there is one
			t.Fatal(err)
		}
		n, err := s.BlankDirETags("pair-a")
		if err != nil || n != 2 {
			t.Fatalf("cached=%v: BlankDirETags = %d, %v; want 2 (the two folders that had an etag)", cached, n, err)
		}
		got, err := s.LoadBaseline("pair-a")
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range []string{"A", "A/deep", "A/blank"} {
			if got[d].RemoteETag != "" {
				t.Errorf("cached=%v: folder %s still has etag %q", cached, d, got[d].RemoteETag)
			}
		}
		if b := got["A/deep"]; b.RemoteFileID != "2" || !b.MountRoot {
			t.Errorf("cached=%v: the rest of a folder row changed: %+v", cached, b)
		}
		if got["A/file.txt"].RemoteETag != "e-file" {
			t.Errorf("cached=%v: a file's etag was blanked", cached)
		}
		other, _ := s.LoadBaseline("pair-b")
		if other["B"].RemoteETag != "e-b" {
			t.Errorf("cached=%v: another pair's folder was blanked", cached)
		}
	}
}
