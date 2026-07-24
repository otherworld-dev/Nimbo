package state

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/otherworld/nimbo/internal/engine"
)

// TestBaselineNoCacheQueries exercises the default low-memory path: every read
// goes straight to the DB (no resident cache), so the direct scoped/paths/empty
// queries must return the same correct results.
func TestBaselineNoCacheQueries(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	st, err := Open(dbPath, "acct", false) // low-memory: direct DB queries
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	for _, r := range []engine.BaselineState{
		{Path: "top.txt", RemoteETag: "e0", LocalSize: 1},
		{Path: "dir/a.txt", RemoteETag: "e1", LocalSize: 2},
		{Path: "dir/sub/b.txt", RemoteETag: "e2", LocalSize: 3},
		{Path: "other/c.txt", RemoteETag: "e3", LocalSize: 4},
	} {
		if err := st.UpsertBaseline("P", r); err != nil {
			t.Fatal(err)
		}
	}
	if full, _ := st.LoadBaseline("P"); len(full) != 4 {
		t.Fatalf("full load: %d rows", len(full))
	}
	if sc, _ := st.LoadBaselineScoped("P", "dir"); len(sc) != 2 || sc["other/c.txt"].Path != "" {
		t.Fatalf("scoped: %+v", sc)
	}
	if pp, _ := st.LoadBaselinePaths("P", []string{"top.txt", "missing", "other/c.txt"}); len(pp) != 2 || pp["top.txt"].RemoteETag != "e0" {
		t.Fatalf("paths: %+v", pp)
	}
	if empty, _ := st.BaselineEmpty("P"); empty {
		t.Fatal("P should not be empty")
	}
	if empty, _ := st.BaselineEmpty("Q"); !empty {
		t.Fatal("Q should be empty")
	}
	if err := st.DeleteBaseline("P", "dir/a.txt"); err != nil {
		t.Fatal(err)
	}
	if full, _ := st.LoadBaseline("P"); len(full) != 3 {
		t.Fatalf("after delete: %d rows", len(full))
	}
}

// TestBaselineCacheConcurrentWrites mirrors many transfer workers recording
// baselines at once: the cache + DB writes must stay race-free and consistent.
func TestBaselineCacheConcurrentWrites(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	st, err := Open(dbPath, "acct", true)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	// Prime the cache so writes hit the resident map (loaded == present).
	if _, err := st.LoadBaseline("P"); err != nil {
		t.Fatalf("prime: %v", err)
	}

	const workers, each = 8, 100
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				p := fmt.Sprintf("w%d/f%d.txt", w, i)
				_ = st.UpsertBaseline("P", engine.BaselineState{Path: p, RemoteETag: "e", LocalSize: int64(i)})
			}
		}(w)
	}
	wg.Wait()

	got, err := st.LoadBaseline("P")
	if err != nil {
		t.Fatalf("LoadBaseline: %v", err)
	}
	if len(got) != workers*each {
		t.Fatalf("got %d baseline rows, want %d", len(got), workers*each)
	}
}

// TestBaselineCacheWriteThrough verifies the resident cache is write-through: a
// second Store opened on the same file sees every write, and scoped/paths reads
// served from the cache return the correct subsets.
func TestBaselineCacheWriteThrough(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	st1, err := Open(dbPath, "acct", true)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	rows := []engine.BaselineState{
		{Path: "top.txt", RemoteETag: "e0", LocalSize: 1},
		{Path: "dir/a.txt", RemoteETag: "e1", LocalSize: 2},
		{Path: "dir/sub/b.txt", RemoteETag: "e2", LocalSize: 3},
		{Path: "other/c.txt", RemoteETag: "e3", LocalSize: 4},
	}
	for _, r := range rows {
		if err := st1.UpsertBaseline("P", r); err != nil {
			t.Fatalf("upsert %s: %v", r.Path, err)
		}
	}
	if err := st1.Close(); err != nil { // flush to disk
		t.Fatalf("close st1: %v", err)
	}

	// Fresh Store (cold cache) must read everything that was written — proving the
	// writes persisted to the DB and weren't only held in st1's cache.
	st2, err := Open(dbPath, "acct", true)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()

	full, err := st2.LoadBaseline("P")
	if err != nil {
		t.Fatalf("LoadBaseline: %v", err)
	}
	if len(full) != 4 || full["dir/sub/b.txt"].RemoteETag != "e2" {
		t.Fatalf("persisted baseline wrong: %+v", full)
	}

	// Scoped read (served from cache) returns only the subtree.
	scoped, err := st2.LoadBaselineScoped("P", "dir")
	if err != nil {
		t.Fatalf("scoped: %v", err)
	}
	if len(scoped) != 2 || scoped["other/c.txt"].Path != "" {
		t.Fatalf("scoped subset wrong: %+v", scoped)
	}

	// Paths read (served from cache) returns exactly the requested existing paths.
	paths, err := st2.LoadBaselinePaths("P", []string{"top.txt", "missing.txt", "other/c.txt"})
	if err != nil {
		t.Fatalf("paths: %v", err)
	}
	if len(paths) != 2 || paths["top.txt"].RemoteETag != "e0" || paths["other/c.txt"].RemoteETag != "e3" {
		t.Fatalf("paths subset wrong: %+v", paths)
	}

	// A cache update must be visible on the next load, and a delete must persist.
	if err := st2.UpsertBaseline("P", engine.BaselineState{Path: "top.txt", RemoteETag: "eX", LocalSize: 9}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if g, _ := st2.LoadBaseline("P"); g["top.txt"].RemoteETag != "eX" {
		t.Fatalf("cache did not reflect update: %+v", g["top.txt"])
	}
	if err := st2.DeleteBaseline("P", "dir/a.txt"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if g, _ := st2.LoadBaseline("P"); len(g) != 3 {
		t.Fatalf("delete not reflected, have %d rows", len(g))
	}
	if empty, _ := st2.BaselineEmpty("P"); empty {
		t.Fatal("BaselineEmpty should be false with rows present")
	}
}
