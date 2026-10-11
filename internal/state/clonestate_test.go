package state

import (
	"reflect"
	"testing"
)

// The folders to download first round-trip in their chosen order, an empty
// list removes them, and clearing a pair's clone state takes them with it
// (only for that pair).
func TestCloneFirst(t *testing.T) {
	s := openTemp(t, false)
	if got, err := s.CloneFirst("pair-a"); err != nil || got != nil {
		t.Fatalf("nothing chosen = %v, %v; want nil, nil", got, err)
	}
	want := []string{"Work/Current", "Photos"}
	if err := s.SetCloneFirst("pair-a", want); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCloneFirst("pair-b", []string{"Music"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.CloneFirst("pair-a"); !reflect.DeepEqual(got, want) {
		t.Fatalf("CloneFirst = %v, want %v", got, want)
	}
	if err := s.SetCloneFirst("pair-a", []string{"Photos"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.CloneFirst("pair-a"); !reflect.DeepEqual(got, []string{"Photos"}) {
		t.Fatalf("after replace = %v, want [Photos]", got)
	}

	if err := s.ClearCloneStatus("pair-a"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.CloneFirst("pair-a"); got != nil {
		t.Errorf("after ClearCloneStatus = %v, want nothing", got)
	}
	if got, _ := s.CloneFirst("pair-b"); !reflect.DeepEqual(got, []string{"Music"}) {
		t.Errorf("unrelated pair = %v, want [Music]", got)
	}

	if err := s.SetCloneFirst("pair-b", nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.CloneFirst("pair-b"); got != nil {
		t.Errorf("after setting an empty list = %v, want nothing", got)
	}
}

// Moving a sync folder re-keys its clone state; the folders chosen to
// download first must follow, or a clone resumed after the move forgets them.
func TestRekeyPairMovesCloneFirst(t *testing.T) {
	s := openTemp(t, false)
	if err := s.SetCloneStatus("old", "started"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCloneFirst("old", []string{"Work"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RekeyPair("old", "new"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.CloneFirst("new"); !reflect.DeepEqual(got, []string{"Work"}) {
		t.Errorf("new key = %v, want [Work]", got)
	}
	if got, _ := s.CloneFirst("old"); got != nil {
		t.Errorf("old key = %v, want nothing", got)
	}
}

// ClearCloneStatus forgets one pair's initial-clone state and nothing else.
// A "started" row that outlives its pair is dangerous: re-adding the folder
// later RESUMES the clone (a differing local file is refetched — overwritten)
// instead of taking it over (a differing local file is never touched).
func TestClearCloneStatus(t *testing.T) {
	s := openTemp(t, false)
	if err := s.SetCloneStatus("pair-a", "started"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCloneStatus("pair-b", "done"); err != nil {
		t.Fatal(err)
	}

	if err := s.ClearCloneStatus("pair-a"); err != nil {
		t.Fatalf("clear: %v", err)
	}

	if got, _ := s.CloneStatus("pair-a"); got != "" {
		t.Errorf("pair-a status after clear = %q, want %q", got, "")
	}
	if got, _ := s.CloneStatus("pair-b"); got != "done" {
		t.Errorf("pair-b status = %q, want %q (an unrelated pair must be untouched)", got, "done")
	}
	// Clearing a pair that has no row is not an error — removal paths call
	// this unconditionally.
	if err := s.ClearCloneStatus("never-seen"); err != nil {
		t.Errorf("clear of an absent row: %v", err)
	}
}
