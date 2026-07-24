package state

import (
	"path/filepath"
	"testing"

	"github.com/otherworld/nimbo/internal/engine"
)

// The batch upsert must insert new rows, replace existing ones, and keep a
// resident cache coherent — it backs the dir-etag maintenance that runs after
// every sync pass.
func TestUpsertBaselineBatch(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	st, err := Open(dbPath, "acct1", true) // cached, to exercise cache coherence
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if err := st.UpsertBaseline("P", engine.BaselineState{Path: "dir", IsDir: true, RemoteETag: "old"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := st.LoadBaseline("P"); err != nil { // populate the resident cache
		t.Fatalf("LoadBaseline: %v", err)
	}

	rows := []engine.BaselineState{
		{Path: "dir", IsDir: true, RemoteETag: "new", RemoteFileID: "f1"},    // update
		{Path: "dir/sub", IsDir: true, RemoteETag: "e2", RemoteFileID: "f2"}, // insert
		{Path: "other", IsDir: true, RemoteETag: "", RemoteFileID: "f3"},     // dirty marker
	}
	if err := st.UpsertBaselineBatch("P", rows); err != nil {
		t.Fatalf("UpsertBaselineBatch: %v", err)
	}

	// Cache sees the writes.
	got, err := st.LoadBaseline("P")
	if err != nil {
		t.Fatalf("LoadBaseline: %v", err)
	}
	if got["dir"].RemoteETag != "new" || got["dir/sub"].RemoteETag != "e2" || got["other"].RemoteETag != "" {
		t.Fatalf("cached view wrong after batch: %+v", got)
	}

	// A fresh store (no cache) sees the same rows on disk.
	st2, err := Open(dbPath, "acct1", false)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	disk, err := st2.LoadBaseline("P")
	if err != nil {
		t.Fatalf("LoadBaseline disk: %v", err)
	}
	if len(disk) != 3 || disk["dir"].RemoteETag != "new" || !disk["dir"].IsDir || disk["dir"].RemoteFileID != "f1" {
		t.Fatalf("persisted view wrong: %+v", disk)
	}

	if err := st.UpsertBaselineBatch("P", nil); err != nil { // empty batch is a no-op
		t.Fatalf("empty batch: %v", err)
	}
}

// The batch delete must remove rows and keep a resident cache coherent.
func TestDeleteBaselineBatch(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	st, err := Open(dbPath, "acct1", true)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	for _, p := range []string{"a", "b", "keep"} {
		if err := st.UpsertBaseline("P", engine.BaselineState{Path: p, RemoteETag: "e"}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if _, err := st.LoadBaseline("P"); err != nil { // populate cache
		t.Fatalf("LoadBaseline: %v", err)
	}
	if err := st.DeleteBaselineBatch("P", []string{"a", "b", "missing"}); err != nil {
		t.Fatalf("DeleteBaselineBatch: %v", err)
	}
	got, _ := st.LoadBaseline("P")
	if len(got) != 1 || got["keep"].RemoteETag != "e" {
		t.Fatalf("cached view wrong after batch delete: %+v", got)
	}
	st2, err := Open(dbPath, "acct1", false)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	disk, _ := st2.LoadBaseline("P")
	if len(disk) != 1 {
		t.Fatalf("persisted view wrong: %+v", disk)
	}
	if err := st.DeleteBaselineBatch("P", nil); err != nil { // no-op
		t.Fatalf("empty batch: %v", err)
	}
}
