package agent

import (
	"path/filepath"
	"sort"
	"testing"
)

func TestRelsFor(t *testing.T) {
	root := filepath.FromSlash("/sync/root")
	abs := func(p string) string { return filepath.Join(root, filepath.FromSlash(p)) }

	t.Run("subfolder file", func(t *testing.T) {
		rels, ok := relsFor(root, []string{abs("Projects/Sub/x.txt")})
		if !ok || len(rels) != 1 || rels[0] != "Projects/Sub/x.txt" {
			t.Fatalf("got %v ok=%v", rels, ok)
		}
	})

	// A root-level file must get the targeted fast path now (the old scopesFor
	// forced these to a full sync because their parent dir was "").
	t.Run("root-level file", func(t *testing.T) {
		rels, ok := relsFor(root, []string{abs("top.txt")})
		if !ok || len(rels) != 1 || rels[0] != "top.txt" {
			t.Fatalf("got %v ok=%v", rels, ok)
		}
	})

	t.Run("dedupes", func(t *testing.T) {
		rels, ok := relsFor(root, []string{abs("a/x"), abs("a/x"), abs("b/y")})
		sort.Strings(rels)
		if !ok || len(rels) != 2 || rels[0] != "a/x" || rels[1] != "b/y" {
			t.Fatalf("got %v ok=%v", rels, ok)
		}
	})

	t.Run("outside the pair falls back", func(t *testing.T) {
		if _, ok := relsFor(root, []string{filepath.Join(root, "..", "other.txt")}); ok {
			t.Fatal("path outside the pair should return ok=false")
		}
	})

	t.Run("empty falls back", func(t *testing.T) {
		if _, ok := relsFor(root, nil); ok {
			t.Fatal("empty change set should return ok=false")
		}
	})

	t.Run("too many falls back", func(t *testing.T) {
		many := make([]string, maxSyncPaths+1)
		for i := range many {
			many[i] = abs(filepath.Join("d", string(rune('a'+i%26)), filepath.FromSlash("f")) + string(rune('0'+i%10)))
		}
		// ensure distinct count exceeds the cap regardless of collisions above
		many = make([]string, 0, maxSyncPaths+1)
		for i := 0; i <= maxSyncPaths; i++ {
			many = append(many, abs(filepath.FromSlash("dir/file"))+string(rune(i)))
		}
		if _, ok := relsFor(root, many); ok {
			t.Fatalf("more than %d paths should fall back to full sync", maxSyncPaths)
		}
	})
}
