package state

import (
	"path/filepath"
	"testing"

	"github.com/otherworld/nimbo/internal/engine"
)

func TestBaselineRoundTrip(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	st, err := Open(dbPath, "acct1", false) // default low-memory (no-cache) path
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	// Empty on first load.
	got, err := st.LoadBaseline("Notes")
	if err != nil {
		t.Fatalf("LoadBaseline empty: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty baseline, got %d", len(got))
	}

	want := engine.BaselineState{
		Path: "a.txt", IsDir: false, RemoteETag: "e1", RemoteFileID: "123",
		LocalSize: 42, LocalMTimeNanos: 1700000000000000000,
	}
	if err := st.UpsertBaseline("Notes", want); err != nil {
		t.Fatalf("UpsertBaseline: %v", err)
	}

	got, err = st.LoadBaseline("Notes")
	if err != nil {
		t.Fatalf("LoadBaseline: %v", err)
	}
	if got["a.txt"] != want {
		t.Errorf("round trip = %+v, want %+v", got["a.txt"], want)
	}

	// Scoped by sync_root: a different root sees nothing.
	if other, _ := st.LoadBaseline("Other"); len(other) != 0 {
		t.Errorf("baseline leaked across sync roots: %v", other)
	}

	// Update replaces in place.
	want.RemoteETag = "e2"
	if err := st.UpsertBaseline("Notes", want); err != nil {
		t.Fatalf("UpsertBaseline update: %v", err)
	}
	got, _ = st.LoadBaseline("Notes")
	if len(got) != 1 || got["a.txt"].RemoteETag != "e2" {
		t.Errorf("update did not replace in place: %+v", got)
	}

	// Delete removes it.
	if err := st.DeleteBaseline("Notes", "a.txt"); err != nil {
		t.Fatalf("DeleteBaseline: %v", err)
	}
	if got, _ = st.LoadBaseline("Notes"); len(got) != 0 {
		t.Errorf("expected empty after delete, got %d", len(got))
	}
}
