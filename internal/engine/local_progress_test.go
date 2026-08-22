package engine

import (
	"os"
	"path/filepath"
	"testing"
)

// mkTree writes the given relative file paths under a fresh temp root.
func mkTree(t *testing.T, rels ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, rel := range rels {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// The scan reports a running count that only ever rises and finishes at the
// number of entries it actually returned — that is what makes it usable as a
// liveness heartbeat for a UI sitting on a multi-minute walk.
func TestLocalScanProgressCountsEveryEntry(t *testing.T) {
	root := mkTree(t, "a/b/c.txt", "a/x.txt", "other/y.txt")

	var seen []int
	got, err := LocalScanProgress(root, "", func(files int) {
		seen = append(seen, files)
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(seen) != len(got) {
		t.Fatalf("progress called %d time(s) for %d entr(ies): %v", len(seen), len(got), seen)
	}
	for i, n := range seen {
		if n != i+1 {
			t.Fatalf("progress must report a running total 1..n, got %v", seen)
		}
	}
	if len(seen) == 0 || seen[len(seen)-1] != len(got) {
		t.Fatalf("final progress %v should equal entry count %d", seen, len(got))
	}
}

// The callback is optional: a nil progress func must behave exactly as the
// plain scan, so the existing call sites need no change.
func TestLocalScanProgressNilCallbackMatchesLocalScan(t *testing.T) {
	root := mkTree(t, "a/b/c.txt", "a/x.txt", "other/y.txt")

	want, err := LocalScan(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := LocalScanProgress(root, "", nil)
	if err != nil {
		t.Fatalf("nil progress must not error: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("nil progress returned %d entries, LocalScan returned %d", len(got), len(want))
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			t.Fatalf("nil progress dropped entry %q", k)
		}
	}
}

// Scoping still applies when a progress func is supplied — the count must cover
// only the walked subtree, not the whole root.
func TestLocalScanProgressRespectsScope(t *testing.T) {
	root := mkTree(t, "a/b/c.txt", "a/x.txt", "other/y.txt")

	var last int
	got, err := LocalScanProgress(root, "a", func(files int) { last = files })
	if err != nil {
		t.Fatal(err)
	}
	if last != len(got) {
		t.Fatalf("final progress %d should equal scoped entry count %d", last, len(got))
	}
	if _, leaked := got["other/y.txt"]; leaked {
		t.Fatal("scoped scan leaked an entry from outside the scope")
	}
}
