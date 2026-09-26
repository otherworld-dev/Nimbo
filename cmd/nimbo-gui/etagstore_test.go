package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// knownDir is the safety gate for the vfs state heal: a plain directory is only
// converted back into a cloud placeholder when the local baselines PROVE the
// server has it. A false positive here stamps "synced" onto something the
// server may not have, so the negative cases matter more than the positives.
func TestKnownDir(t *testing.T) {
	s := newEtagStore(filepath.Join(t.TempDir(), "etags.json"))
	s.setMany(map[string]string{
		"Documents":            "e1",
		"Documents/Report.doc": "e2",
		"Photos/2026/cat.jpg":  "e3",
	})

	for _, tc := range []struct {
		remote string
		want   bool
		why    string
	}{
		{"Documents", true, "has its own baseline"},
		{"/Documents/", true, "slashes are trimmed like every other accessor"},
		{"Photos", true, "no own baseline, but a child baseline proves it exists"},
		{"Photos/2026", true, "intermediate ancestor of a recorded file"},
		{"Photo", false, "prefix of a sibling name must NOT match (Photos)"},
		{"Documents/Report.doc", true, "a recorded path counts even if it is a file"},
		{"Backups", false, "nothing recorded beneath it"},
		{"", false, "the root is never the heal's business"},
	} {
		if got := s.knownDir(tc.remote); got != tc.want {
			t.Errorf("knownDir(%q) = %v, want %v (%s)", tc.remote, got, tc.want, tc.why)
		}
	}
}

// moveMany is the store side of a moved subtree's baseline carry — one call,
// one persist, because every write here marshals and rewrites the entire file.
// (That there is exactly one call per move is pinned on the watcher side, in
// TestDirectoryMoveCarriesDescendantBaselinesInOneWrite; this pins what the
// call does.)
func TestMoveMany(t *testing.T) {
	path := filepath.Join(t.TempDir(), "etags.json")
	s := newEtagStore(path)
	s.setMany(map[string]string{
		"d":             "e-dir",
		"d/x.txt":       "e-x",
		"d/inner/y.txt": "e-y",
		"other.txt":     "e-o",
	})

	s.moveMany([][2]string{
		{"d", "e"},
		{"d/x.txt", "e/x.txt"},
		{"d/inner/y.txt", "e/inner/y.txt"},
		{"d/gone.txt", "e/gone.txt"}, // nothing recorded: carries nothing
		{"same", "same"},             // a no-op pair
	})

	for remote, want := range map[string]string{
		"e": "e-dir", "e/x.txt": "e-x", "e/inner/y.txt": "e-y", "other.txt": "e-o",
	} {
		if got := s.get(remote); got != want {
			t.Errorf("get(%q) = %q, want %q", remote, got, want)
		}
	}
	for _, gone := range []string{"d", "d/x.txt", "d/inner/y.txt"} {
		if got := s.get(gone); got != "" {
			t.Errorf("get(%q) = %q — the vacated name must be dropped, or a file later created there inherits it", gone, got)
		}
	}
	// The carry is persisted, not just in memory: a restart must see it.
	if got := newEtagStore(path).get("e/inner/y.txt"); got != "e-y" {
		t.Errorf("after reload, get(%q) = %q, want e-y", "e/inner/y.txt", got)
	}
}

// A carry that changes nothing must not rewrite the file at all.
func TestMoveManyWithNothingToCarryDoesNotPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "etags.json")
	s := newEtagStore(path)
	s.moveMany([][2]string{{"a", "b"}, {"c", "c"}})
	if _, err := os.Stat(path); err == nil {
		t.Error("the store wrote its file for a carry that moved nothing")
	}
}

// A blank destination must not be read as "forget the source": losing a
// baseline costs a spurious conflicted copy the next time that file is edited.
func TestMoveManyKeepsABaselineWithNowhereToGo(t *testing.T) {
	s := newEtagStore(filepath.Join(t.TempDir(), "etags.json"))
	s.setMany(map[string]string{"d/x.txt": "e-x"})
	s.moveMany([][2]string{{"d/x.txt", ""}, {"d/x.txt", "/"}})
	if got := s.get("d/x.txt"); got != "e-x" {
		t.Errorf("get(%q) = %q, want e-x", "d/x.txt", got)
	}
}

// Content keys (GitHub #7) live beside the ETag baselines and must follow them
// through every move and forget, and survive a restart: a key left behind at
// an old path, or lost, costs a false conflict the next time a colleague locks
// the file.
func TestEtagStoreContentKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vfs-etags.json")
	s := newEtagStore(path)
	s.setMany(map[string]string{"a.txt": "e1", "d/b.txt": "e2", "d/c.txt": "e3"})
	s.setContentKeys(map[string]string{"a.txt": "k1", "d/b.txt": "k2", "d/c.txt": "k3"})

	if got := s.contentKey("/a.txt"); got != "k1" {
		t.Fatalf("contentKey(a.txt) = %q, want k1", got)
	}
	s.setContentKeys(map[string]string{"a.txt": ""}) // "" clears
	if got := s.contentKey("a.txt"); got != "" {
		t.Errorf("cleared key still %q", got)
	}
	s.moveMany([][2]string{{"d/b.txt", "e/b.txt"}})
	if s.contentKey("d/b.txt") != "" || s.contentKey("e/b.txt") != "k2" {
		t.Errorf("move did not carry the key: old=%q new=%q", s.contentKey("d/b.txt"), s.contentKey("e/b.txt"))
	}
	// A move onto a path with an old key, from one with none, must not leave
	// the old key describing the arriving file.
	s.setMany(map[string]string{"n.txt": "en", "old.txt": "eo"})
	s.setContentKeys(map[string]string{"old.txt": "k-old"})
	s.moveMany([][2]string{{"n.txt", "old.txt"}})
	if got := s.contentKey("old.txt"); got != "" {
		t.Errorf("move from a keyless source left the destination's old key %q", got)
	}
	s.del("e/b.txt")
	if s.contentKey("e/b.txt") != "" {
		t.Error("del left the key behind")
	}
	s.setContentKeys(map[string]string{"d/x.txt": "kx"})
	s.delUnder("d")
	if s.contentKey("d/c.txt") != "" || s.contentKey("d/x.txt") != "" {
		t.Error("delUnder left keys behind")
	}
	s.setContentKeys(map[string]string{"z.txt": "kz"})

	re := newEtagStore(path)
	if re.contentKey("z.txt") != "kz" {
		t.Errorf("key not persisted: %q", re.contentKey("z.txt"))
	}
	// The ETag file keeps its old format (a flat path -> ETag map), so an older
	// Nimbo that ignores the key file still reads its baselines.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var flat map[string]string
	if err := json.Unmarshal(b, &flat); err != nil || flat["z.txt"] != "" || len(flat) == 0 {
		t.Errorf("ETag file no longer a flat map (err=%v): %s", err, b)
	}
}
