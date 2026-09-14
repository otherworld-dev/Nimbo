package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The flyout's recent-activity rows open the file's folder in Explorer. The
// activity feed records two path shapes — live-sync events are pair-relative,
// on-demand (VFS) events carry the mount's remote root as a prefix — so the
// local-path derivation has to know which it is given, and a rename records
// "old → new" where only the destination still exists.
func TestActivityLocalPath(t *testing.T) {
	local := filepath.Join("C:", string(filepath.Separator), "Users", "me", "Nimbo")
	cases := []struct {
		name, remoteRoot, path, want string
	}{
		{"live pair-relative", "", "Docs/notes.txt", filepath.Join(local, "Docs", "notes.txt")},
		{"live nested root-lookalike is NOT stripped", "", "Nimbo/x.txt", filepath.Join(local, "Nimbo", "x.txt")},
		{"vfs strips the mount's remote root", "Documents", "Documents/sub/a.pdf", filepath.Join(local, "sub", "a.pdf")},
		{"vfs nested remote root", "Work/Projects", "Work/Projects/p1/main.go", filepath.Join(local, "p1", "main.go")},
		{"vfs mount of the whole account", "", "Photos/img.jpg", filepath.Join(local, "Photos", "img.jpg")},
		{"vfs root prefix must match a whole segment", "Doc", "Documents/a.txt", filepath.Join(local, "Documents", "a.txt")},
		{"move records old → new; the destination is what exists", "", "a/old.txt → b/new.txt", filepath.Join(local, "b", "new.txt")},
		{"vfs move", "Documents", "Documents/a.txt → Documents/b/a.txt", filepath.Join(local, "b", "a.txt")},
		{"leading slash tolerated", "", "/Docs/notes.txt", filepath.Join(local, "Docs", "notes.txt")},
		{"empty path is the folder itself", "", "", local},
	}
	for _, c := range cases {
		if got := activityLocalPath(local, c.remoteRoot, c.path); got != c.want {
			t.Errorf("%s: activityLocalPath(%q, %q, %q) = %q, want %q", c.name, local, c.remoteRoot, c.path, got, c.want)
		}
	}
	// No owning folder (a synthetic event such as a parked server copy) → no
	// local path, so the row falls back to opening the Sync status window.
	if got := activityLocalPath("", "", "x.txt"); got != "" {
		t.Errorf("no local dir: got %q, want empty", got)
	}
}

func TestRevealTarget(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Docs", "deep")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(file, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	// An existing file is revealed (selected in its folder).
	if got, sel := revealTarget(file); got != file || !sel {
		t.Errorf("existing file: got (%q, %v), want (%q, true)", got, sel, file)
	}
	// An existing directory (a "New folder" event) is revealed the same way.
	if got, sel := revealTarget(dir); got != dir || !sel {
		t.Errorf("existing dir: got (%q, %v), want (%q, true)", got, sel, dir)
	}
	// A deleted file opens the folder it was in.
	gone := filepath.Join(dir, "deleted.txt")
	if got, sel := revealTarget(gone); got != dir || sel {
		t.Errorf("deleted file: got (%q, %v), want (%q, false)", got, sel, dir)
	}
	// A deleted subtree opens the nearest ancestor that still exists.
	deep := filepath.Join(dir, "a", "b", "c.txt")
	if got, sel := revealTarget(deep); got != dir || sel {
		t.Errorf("deleted subtree: got (%q, %v), want (%q, false)", got, sel, dir)
	}
	// Nothing left below the volume root (the sync folder itself is gone):
	// there is nothing useful to show, and the drive root is not it.
	if got, _ := revealTarget(filepath.Join(filepath.VolumeName(root)+string(filepath.Separator), "nope-"+filepath.Base(root), "x.txt")); got != "" {
		t.Errorf("vanished root: got %q, want empty", got)
	}
	if got, _ := revealTarget(""); got != "" {
		t.Errorf("empty path: got %q, want empty", got)
	}
}
