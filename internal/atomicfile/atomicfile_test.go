package atomicfile

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// stubRename swaps the OS rename for one that fails the way we need. Windows
// cannot be asked to fail a same-directory rename on demand, and the whole
// point of this package is what happens when it does.
func stubRename(t *testing.T, err error) {
	t.Helper()
	rename = func(string, string) error { return err }
	t.Cleanup(func() { rename = os.Rename })
}

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// GitHub issue #8: a first sign-in died with "commit account store: rename
// ...accounts.json.tmp ...accounts.json: The system cannot move the file to a
// different disk drive". Both names live in one directory, so Windows can only
// call that a cross-drive move if it resolved them onto different volumes —
// a packaged build's %AppData% is virtualised into the package's LocalCache,
// so the temp file and its final name need not share a disk. Nothing else in
// the app worked until this commit did.
func TestCommitWritesThroughWhenRenameCrossesVolumes(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "accounts.json.tmp")
	dst := filepath.Join(dir, "accounts.json")
	write(t, tmp, "new")
	write(t, dst, "old")
	stubRename(t, &os.LinkError{Op: "rename", Old: tmp, New: dst, Err: crossVolumeErr()})

	if err := Commit(tmp, dst); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := read(t, dst); got != "new" {
		t.Errorf("destination = %q, want %q", got, "new")
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("temp file survived the commit, want it gone (stat err: %v)", err)
	}
}

// Only a cross-volume failure earns the non-atomic fallback. Anything else —
// a locked target, a vanished directory — is a real failure and must surface,
// with the old contents left alone.
func TestCommitReportsOtherRenameFailures(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "accounts.json.tmp")
	dst := filepath.Join(dir, "accounts.json")
	write(t, tmp, "new")
	write(t, dst, "old")
	stubRename(t, &os.LinkError{Op: "rename", Old: tmp, New: dst, Err: os.ErrPermission})

	if err := Commit(tmp, dst); err == nil {
		t.Fatal("Commit succeeded, want the permission error surfaced")
	}
	if got := read(t, dst); got != "old" {
		t.Errorf("destination = %q, want it untouched (%q)", got, "old")
	}
}

// The ordinary path stays an atomic rename: no fallback, no copying.
func TestCommitRenamesWhenItCan(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "accounts.json.tmp")
	dst := filepath.Join(dir, "accounts.json")
	write(t, tmp, "new")

	if err := Commit(tmp, dst); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := read(t, dst); got != "new" {
		t.Errorf("destination = %q, want %q", got, "new")
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("temp file survived the commit, want it gone (stat err: %v)", err)
	}
}

// Config files are written 0600 and hold account metadata; the fallback must
// not widen that. Windows models only the read-only bit, so this is a
// Unix/Android concern.
func TestCommitFallbackKeepsFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not model Unix permission bits")
	}
	dir := t.TempDir()
	tmp := filepath.Join(dir, "accounts.json.tmp")
	dst := filepath.Join(dir, "accounts.json")
	write(t, tmp, "new")
	stubRename(t, &os.LinkError{Op: "rename", Old: tmp, New: dst, Err: crossVolumeErr()})

	if err := Commit(tmp, dst); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat %s: %v", dst, err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("destination mode = %v, want 0600", got)
	}
}
