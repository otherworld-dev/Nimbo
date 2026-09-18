package state

import (
	"database/sql"
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

// The share/mount-root flag is what a later pass uses to tell an unshare from a
// deletion, so it must survive the round trip through every write path and
// both read modes.
func TestBaselineMountRootPersists(t *testing.T) {
	for _, cached := range []bool{false, true} {
		dbPath := filepath.Join(t.TempDir(), "state.db")
		st, err := Open(dbPath, "acct1", cached)
		if err != nil {
			t.Fatalf("Open(cached=%v): %v", cached, err)
		}
		single := engine.BaselineState{Path: "Team", IsDir: true, RemoteETag: "e1", MountRoot: true}
		if err := st.UpsertBaseline("P", single); err != nil {
			t.Fatal(err)
		}
		batch := []engine.BaselineState{
			{Path: "Budget.xlsx", RemoteETag: "e2", MountRoot: true},
			{Path: "own", IsDir: true, RemoteETag: "e3"},
		}
		if err := st.UpsertBaselineBatch("P", batch); err != nil {
			t.Fatal(err)
		}
		st.Close()

		// A fresh handle reads from disk, whatever the cache mode.
		st, err = Open(dbPath, "acct1", cached)
		if err != nil {
			t.Fatal(err)
		}
		got, err := st.LoadBaseline("P")
		if err != nil {
			t.Fatal(err)
		}
		if got["Team"] != single {
			t.Errorf("cached=%v: single upsert = %+v, want %+v", cached, got["Team"], single)
		}
		if !got["Budget.xlsx"].MountRoot || got["own"].MountRoot {
			t.Errorf("cached=%v: batch flags wrong: %+v / %+v", cached, got["Budget.xlsx"], got["own"])
		}
		paths, _ := st.LoadBaselinePaths("P", []string{"Team"})
		if !paths["Team"].MountRoot {
			t.Errorf("cached=%v: LoadBaselinePaths dropped the flag", cached)
		}
		st.Close()
	}
}

// Installs in the field have a baseline table created before mount_root
// existed. CREATE TABLE IF NOT EXISTS leaves that table exactly as it was, so
// Open must add the column itself — and read the old rows as "not a root".
func TestOpenAddsMountRootToAnOlderDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	old, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE baseline (
  account_id        TEXT    NOT NULL,
  pair_key          TEXT    NOT NULL,
  path              TEXT    NOT NULL,
  is_dir            INTEGER NOT NULL,
  remote_etag       TEXT    NOT NULL,
  remote_fileid     TEXT    NOT NULL,
  local_size        INTEGER NOT NULL,
  local_mtime_nanos INTEGER NOT NULL,
  content_sha1      TEXT    NOT NULL DEFAULT '',
  PRIMARY KEY (account_id, pair_key, path)
)`); err != nil {
		t.Fatal(err)
	}
	for _, row := range [][]any{
		{"old.txt", 0, "e"},
		{"Shared", 1, "e-top"},         // a top-level dir: re-listed once so shares under it get flagged
		{"Shared/Team", 1, "e-nested"}, // nested: left alone (its parent's re-listing marks it)
	} {
		if _, err := old.Exec(`INSERT INTO baseline VALUES ('acct1', 'P', ?, ?, ?, 'f', 1, 2, '')`, row...); err != nil {
			t.Fatal(err)
		}
	}
	old.Close()

	st, err := Open(dbPath, "acct1", false)
	if err != nil {
		t.Fatalf("Open on an older database: %v", err)
	}
	defer st.Close()
	got, err := st.LoadBaseline("P")
	if err != nil {
		t.Fatalf("LoadBaseline after migration: %v", err)
	}
	if b, ok := got["old.txt"]; !ok || b.MountRoot || b.RemoteETag != "e" {
		t.Errorf("old row misread after migration: %+v (present=%v)", b, ok)
	}
	// The flag can only be learned by LISTING a share's parent, and the ETag
	// prune skips an unchanged parent forever. Migration therefore dirties the
	// top-level directories (one PROPFIND each, once) so the next pass stamps
	// every share directly under them; deeper rows are not worth a full crawl.
	if got["Shared"].RemoteETag != "" {
		t.Errorf("top-level dir not dirtied for a one-off re-list: %+v", got["Shared"])
	}
	if got["Shared/Team"].RemoteETag != "e-nested" {
		t.Errorf("nested dir should be untouched: %+v", got["Shared/Team"])
	}
	if err := st.UpsertBaseline("P", engine.BaselineState{Path: "Team", IsDir: true, MountRoot: true}); err != nil {
		t.Fatalf("upsert after migration: %v", err)
	}
	got, _ = st.LoadBaseline("P")
	if !got["Team"].MountRoot {
		t.Errorf("flag not stored after migration: %+v", got["Team"])
	}
	// Opening again must not trip over the column it already added.
	st2, err := Open(dbPath, "acct1", false)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	st2.Close()
}

// TestDeleteBaselineAll pins the "start from scratch" safety contract: wiping a
// pair's ENTIRE baseline before its local files are deleted is what makes the
// engine later read the empty folder as "download everything" instead of
// "the user deleted everything - propagate the deletes to the server".
func TestDeleteBaselineAll(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	st, err := Open(dbPath, "acct1", true) // cached path, so the cache purge is exercised too
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	for _, p := range []string{"a.txt", "sub/b.txt", "sub/deep/c.txt"} {
		if err := st.UpsertBaseline("Pair", engine.BaselineState{Path: p, RemoteETag: "e"}); err != nil {
			t.Fatalf("UpsertBaseline(%s): %v", p, err)
		}
	}
	if err := st.UpsertBaseline("OtherPair", engine.BaselineState{Path: "keep.txt", RemoteETag: "e"}); err != nil {
		t.Fatalf("UpsertBaseline(other): %v", err)
	}

	if err := st.DeleteBaselineAll("Pair"); err != nil {
		t.Fatalf("DeleteBaselineAll: %v", err)
	}
	if got, _ := st.LoadBaseline("Pair"); len(got) != 0 {
		t.Errorf("baseline not fully wiped: %v", got)
	}
	if got, _ := st.LoadBaseline("OtherPair"); len(got) != 1 {
		t.Errorf("another pair's baseline was touched: %v", got)
	}
}
