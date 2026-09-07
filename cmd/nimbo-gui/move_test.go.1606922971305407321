package main

import (
	"os"
	"path/filepath"
	"testing"
)

func mkfile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// On the same volume a move is an atomic rename: content arrives at dst, src is gone.
func TestRelocateFolderSameVolumeRename(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	dst := filepath.Join(base, "moved", "dst")
	mkfile(t, filepath.Join(src, "a.txt"), "hello")
	mkfile(t, filepath.Join(src, "sub", "b.txt"), "world")

	if msg := relocateFolder(src, dst); msg != "" {
		t.Fatalf("relocateFolder returned error: %s", msg)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("source should be gone after the move; stat err = %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "sub", "b.txt")); err != nil || string(b) != "world" {
		t.Errorf("moved content missing/wrong: %q, %v", b, err)
	}
}

// A non-empty destination is refused and the source is left untouched.
func TestRelocateFolderRefusesNonEmptyDest(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	mkfile(t, filepath.Join(src, "a.txt"), "x")
	dst := filepath.Join(base, "dst")
	mkfile(t, filepath.Join(dst, "existing.txt"), "keep")

	if msg := relocateFolder(src, dst); msg == "" {
		t.Fatal("relocateFolder must refuse a non-empty destination")
	}
	if _, err := os.Stat(filepath.Join(src, "a.txt")); err != nil {
		t.Errorf("source must be untouched when the move is refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "existing.txt")); err != nil {
		t.Errorf("destination's existing content must be untouched: %v", err)
	}
}

// verifyCopy passes for a faithful copy and fails for a missing file or a size
// mismatch — the gate that prevents a cross-volume move deleting the source
// before the copy is proven good.
func TestVerifyCopy(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	mkfile(t, filepath.Join(src, "a.txt"), "alpha")
	mkfile(t, filepath.Join(src, "d", "b.txt"), "bravo-bravo")

	good := filepath.Join(base, "good")
	if err := copyTree(src, good); err != nil {
		t.Fatal(err)
	}
	if err := verifyCopy(src, good); err != nil {
		t.Errorf("verifyCopy on a faithful copy should pass, got: %v", err)
	}

	missing := filepath.Join(base, "missing")
	if err := copyTree(src, missing); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(missing, "d", "b.txt")); err != nil {
		t.Fatal(err)
	}
	if err := verifyCopy(src, missing); err == nil {
		t.Error("verifyCopy must fail when the copy is missing a file")
	}

	short := filepath.Join(base, "short")
	if err := copyTree(src, short); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(short, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyCopy(src, short); err == nil {
		t.Error("verifyCopy must fail on a size mismatch")
	}
}

// dirSize sums file bytes (and ignores directories).
func TestDirSize(t *testing.T) {
	base := t.TempDir()
	mkfile(t, filepath.Join(base, "a.txt"), "12345")    // 5
	mkfile(t, filepath.Join(base, "d", "b.txt"), "678")  // 3
	if got := dirSize(base); got != 8 {
		t.Errorf("dirSize = %d, want 8", got)
	}
}
