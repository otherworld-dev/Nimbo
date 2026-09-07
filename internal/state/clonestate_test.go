package state

import "testing"

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
