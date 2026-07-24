package engine

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestLocalScanScoped(t *testing.T) {
	root := t.TempDir()
	mk := func(rel string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("a/b/c.txt")
	mk("a/x.txt")
	mk("other/y.txt")

	got, err := LocalScanScoped(root, "a")
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// Keys are pair-relative (prefixed with "a/"), only the "a" subtree is walked,
	// and the scope dir "a" itself is excluded.
	want := []string{"a/b", "a/b/c.txt", "a/x.txt"}
	if len(keys) != len(want) {
		t.Fatalf("got %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("got %v, want %v", keys, want)
		}
	}

	// A scope whose directory doesn't exist (deleted subtree) yields an empty map,
	// not an error, so the diff can propagate the deletions.
	gone, err := LocalScanScoped(root, "nope/missing")
	if err != nil {
		t.Fatalf("missing scope should not error: %v", err)
	}
	if len(gone) != 0 {
		t.Fatalf("missing scope should be empty, got %v", gone)
	}

	// scope "" behaves like a full scan.
	full, err := LocalScanScoped(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := full["other/y.txt"]; !ok {
		t.Errorf("full scan should include other/y.txt")
	}
}
