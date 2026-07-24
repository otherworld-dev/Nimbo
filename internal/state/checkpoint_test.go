package state

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/otherworld/nimbo/internal/engine"
)

func openCPStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "state.db"), "acct1", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func cpCount(t *testing.T, st *Store, pairKey string) int {
	t.Helper()
	var n int
	if err := st.db.QueryRow(
		`SELECT COUNT(*) FROM scan_checkpoint WHERE account_id = ? AND pair_key = ?`,
		st.accountID, pairKey,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestScanDirRoundtrip(t *testing.T) {
	st := openCPStore(t)
	if err := st.SaveScanDir("pk", "Work/dir_a", "etag1", 1, []byte("blob1")); err != nil {
		t.Fatal(err)
	}
	fmtv, blob, ok, err := st.LoadScanDir("pk", "Work/dir_a", "etag1")
	if err != nil || !ok || fmtv != 1 || string(blob) != "blob1" {
		t.Fatalf("hit = (%d, %q, %v, %v), want (1, blob1, true, nil)", fmtv, blob, ok, err)
	}
	// Wrong etag: miss, nil error (the SQL filters on etag).
	if _, _, ok, err := st.LoadScanDir("pk", "Work/dir_a", "other"); ok || err != nil {
		t.Fatalf("etag mismatch: ok=%v err=%v, want miss", ok, err)
	}
	// Absent row: miss.
	if _, _, ok, err := st.LoadScanDir("pk", "nope", "etag1"); ok || err != nil {
		t.Fatalf("absent: ok=%v err=%v, want miss", ok, err)
	}
	// Upsert replaces: old etag now misses, new hits.
	if err := st.SaveScanDir("pk", "Work/dir_a", "etag2", 1, []byte("blob2")); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := st.LoadScanDir("pk", "Work/dir_a", "etag1"); ok {
		t.Fatal("stale etag still hits after upsert")
	}
	if _, blob, ok, _ := st.LoadScanDir("pk", "Work/dir_a", "etag2"); !ok || string(blob) != "blob2" {
		t.Fatal("upserted row missing")
	}
	// LIKE metacharacters in dir_path are inert (exact-match PK lookups only).
	if err := st.SaveScanDir("pk", "100%_done/dir", "e", 1, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := st.LoadScanDir("pk", "100%_done/dir", "e"); !ok {
		t.Fatal("metachar path row missing")
	}
}

func TestClearScanCheckpointBatches(t *testing.T) {
	st := openCPStore(t)
	// >2 batches of 1000 to exercise the bounded-delete loop.
	for i := 0; i < 2500; i++ {
		if err := st.SaveScanDir("pk", fmt.Sprintf("dir/%04d", i), "e", 1, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SaveScanDir("other", "dir/keep", "e", 1, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := st.ClearScanCheckpoint("pk"); err != nil {
		t.Fatal(err)
	}
	if n := cpCount(t, st, "pk"); n != 0 {
		t.Fatalf("%d rows left after clear", n)
	}
	if n := cpCount(t, st, "other"); n != 1 {
		t.Fatalf("clear leaked into another pair: %d rows", n)
	}
}

func TestDeleteScanCheckpointBefore(t *testing.T) {
	st := openCPStore(t)
	if err := st.SaveScanDir("pk", "old", "e", 1, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveScanDir("pk", "fresh", "e", 1, []byte("x")); err != nil {
		t.Fatal(err)
	}
	// Backdate one row 20 days.
	if _, err := st.db.Exec(
		`UPDATE scan_checkpoint SET saved_at = ? WHERE dir_path = 'old'`,
		time.Now().Add(-20*24*time.Hour).Unix(),
	); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteScanCheckpointBefore(time.Now().Add(-14 * 24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := st.LoadScanDir("pk", "old", "e"); ok {
		t.Fatal("aged row survived")
	}
	if _, _, ok, _ := st.LoadScanDir("pk", "fresh", "e"); !ok {
		t.Fatal("fresh row deleted")
	}
}

func TestRekeyPairDropsCheckpointRows(t *testing.T) {
	st := openCPStore(t)
	if err := st.UpsertBaseline("old", engine.BaselineState{Path: "f", RemoteETag: "e"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveScanDir("old", "dir", "e", 1, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := st.RekeyPair("old", "new"); err != nil {
		t.Fatal(err)
	}
	if n := cpCount(t, st, "old"); n != 0 {
		t.Fatal("old-key checkpoint rows survived rekey")
	}
	if n := cpCount(t, st, "new"); n != 0 {
		t.Fatal("checkpoint rows were migrated; they must be dropped (cache, remote root may differ)")
	}
	b, err := st.LoadBaseline("new")
	if err != nil || len(b) != 1 {
		t.Fatalf("baseline did not move: %v %v", b, err)
	}
}

func TestScanDirClosedStore(t *testing.T) {
	st := openCPStore(t)
	st.Close()
	if err := st.SaveScanDir("pk", "dir", "e", 1, []byte("x")); err == nil {
		t.Fatal("Save on closed store: want error")
	}
	if _, _, _, err := st.LoadScanDir("pk", "dir", "e"); err == nil {
		t.Fatal("Load on closed store: want error")
	}
}
