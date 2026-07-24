package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLocalRootVanished covers the data-loss guard's detector: a missing or empty
// local root must read as "vanished" (so syncs refuse to delete server files),
// while a root holding any entry must not.
func TestLocalRootVanished(t *testing.T) {
	base := t.TempDir()

	missing := filepath.Join(base, "never-existed")
	if !localRootVanished(missing) {
		t.Errorf("missing dir: got vanished=false, want true")
	}

	empty := filepath.Join(base, "empty")
	if err := os.Mkdir(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	if !localRootVanished(empty) {
		t.Errorf("empty dir: got vanished=false, want true")
	}

	populated := filepath.Join(base, "populated")
	if err := os.Mkdir(populated, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(populated, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if localRootVanished(populated) {
		t.Errorf("populated dir: got vanished=true, want false")
	}

	// A subdirectory alone (no files) still counts as a present tree.
	withSubdir := filepath.Join(base, "withsubdir")
	if err := os.MkdirAll(filepath.Join(withSubdir, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if localRootVanished(withSubdir) {
		t.Errorf("dir with a subdir: got vanished=true, want false")
	}
}

// TestBulkDeleteGuardTrips covers the fraction-based circuit-breaker: a pass that
// would delete a large share of a pair's files is refused, while small or partial
// deletes pass.
func TestBulkDeleteGuardTrips(t *testing.T) {
	cases := []struct {
		name           string
		deletes, total int
		want           bool
	}{
		{"folder vanished: all of a large pair", 800, 800, true},
		{"exactly floor and half", 50, 100, true},
		{"60% of a large pair", 600, 1000, true},
		{"just under the floor", 49, 49, false},
		{"big pair, below floor", 40, 1000, false},
		{"49 deleted (below floor)", 49, 100, false},
		{"large fraction but tiny pair", 10, 12, false},
		{"above floor, small fraction (6%)", 60, 1000, false},
		{"zero deletes", 0, 1000, false},
	}
	for _, c := range cases {
		if got := bulkDeleteGuardTrips(c.deletes, c.total); got != c.want {
			t.Errorf("%s: bulkDeleteGuardTrips(%d, %d) = %v, want %v", c.name, c.deletes, c.total, got, c.want)
		}
	}
}
