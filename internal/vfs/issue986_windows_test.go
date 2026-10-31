//go:build windows

package vfs

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/otherworld/nimbo/internal/cfapi"
)

// Deck #986 (from GitHub #7's screenshots): in on-demand mode a folder that
// vanished from the server listing was removed locally with os.RemoveAll, so
// an edit inside it that had not uploaded yet was destroyed, and a plain server
// RENAME (detected for files only) threw away every downloaded file too.

func shortSuppression(t *testing.T) {
	t.Helper()
	old := suppressFor
	suppressFor = 200 * time.Millisecond
	t.Cleanup(func() { suppressFor = old })
}

func waitUpload(t *testing.T, rec *recorder, want string) {
	t.Helper()
	deadline := time.After(6 * time.Second)
	for {
		select {
		case got := <-rec.uploaded:
			if got == want {
				return
			}
		case <-deadline:
			t.Fatalf("%s was never uploaded", want)
		}
	}
}

// A folder renamed on the server ("8days" -> "8 days") is renamed here, with
// everything in it: an unsent edit is kept and sent under the new path, and
// nothing is deleted or downloaded again.
func TestReconcilePullsServerFolderRename(t *testing.T) {
	shortSuppression(t)
	f := installFakeCf(t)
	root := t.TempDir()
	old := filepath.Join(root, "Matti 8days")
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(old, "a.txt")
	edit := filepath.Join(old, "notes.txt")
	if err := os.WriteFile(stub, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(edit, []byte("edited, not yet uploaded"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.setIdentity(old, "Matti 8days")
	f.setIdentity(stub, "Matti 8days/a.txt")
	f.setIdentity(edit, "Matti 8days/notes.txt")
	// The fake keys placeholder state by path; on Windows it lives on the file
	// and moves with it, so mark the post-rename paths too.
	nf := filepath.Join(root, "Matti 8 days")
	for _, p := range []string{stub, filepath.Join(nf, "a.txt")} {
		f.markDehydrated(p)
	}
	for _, p := range []string{edit, filepath.Join(nf, "notes.txt")} {
		f.markDirty(p)
		f.markModified(p)
	}
	f.setIdentity(filepath.Join(nf, "a.txt"), "Matti 8days/a.txt")
	f.setIdentity(filepath.Join(nf, "notes.txt"), "Matti 8days/notes.txt")

	rec := newRecorder()
	rec.fileids["Matti 8days"] = "fd1"
	rec.fileids["Matti 8days/a.txt"] = "fa"
	rec.baselines["Matti 8days/a.txt"] = "e-a"
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("Matti 8 days", true, "e-dir2", "fd1")}
	a := ph("a.txt", false, "e-a", "fa")
	a.Identity = []byte("Matti 8 days/a.txt")
	rec.listing["Matti 8 days"] = []cfapi.PlaceholderInfo{a}
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	if got := dirNames(t, root); len(got) != 1 || got[0] != "Matti 8 days" {
		t.Fatalf("local root = %q, want the folder renamed", got)
	}
	if b, err := os.ReadFile(filepath.Join(nf, "notes.txt")); err != nil || string(b) != "edited, not yet uploaded" {
		t.Fatalf("the unsent edit did not survive the rename: %q, %v", b, err)
	}
	if id, _ := f.identityOf(filepath.Join(nf, "a.txt")); id != "Matti 8 days/a.txt" {
		t.Errorf("child identity = %q, want it repointed", id)
	}
	waitUpload(t, rec, "Matti 8 days/notes.txt")
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.deletes) != 0 || len(rec.moves) != 0 {
		t.Errorf("server ops for a rename the server already made: deletes %v, moves %v", rec.deletes, rec.moves)
	}
	if rec.fileids["Matti 8 days"] != "fd1" || rec.fileids["Matti 8 days/a.txt"] != "fa" {
		t.Errorf("fileids not carried to the new path: %v", rec.fileids)
	}
	if _, ok := rec.fileids["Matti 8days"]; ok {
		t.Error("stale fileid left under the old path")
	}
	if rec.baselines["Matti 8 days/a.txt"] != "e-a" {
		t.Errorf("baseline not carried: %v", rec.baselines)
	}
	for _, u := range rec.uploads {
		if u == "Matti 8days/notes.txt" {
			t.Error("the edit was sent under the OLD path, recreating the folder on the server")
		}
	}
}

// A folder deleted (or moved elsewhere) on the server: everything that can be
// fetched again goes, but a file with changes this PC never sent is kept and
// sent, never destroyed.
func TestReconcileServerFolderDeleteKeepsUnsentEdits(t *testing.T) {
	shortSuppression(t)
	f := installFakeCf(t)
	root := t.TempDir()
	gone := filepath.Join(root, "gone")
	sub := filepath.Join(gone, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	clean := filepath.Join(gone, "clean.txt")
	stub := filepath.Join(gone, "stub.txt")
	edit := filepath.Join(sub, "edit.txt")
	for p, b := range map[string]string{clean: "server copy", stub: "", edit: "my change"} {
		if err := os.WriteFile(p, []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for p, id := range map[string]string{gone: "gone", sub: "gone/sub", clean: "gone/clean.txt", stub: "gone/stub.txt", edit: "gone/sub/edit.txt"} {
		f.setIdentity(p, id)
	}
	f.markDehydrated(stub)
	f.markDirty(edit)
	f.markModified(edit)

	rec := newRecorder()
	rec.listing[""] = nil // the server no longer has "gone"
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	if b, err := os.ReadFile(edit); err != nil || string(b) != "my change" {
		t.Fatalf("the unsent edit was destroyed: %q, %v", b, err)
	}
	for _, p := range []string{clean, stub} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s kept: nothing in it was local-only", filepath.Base(p))
		}
	}
	waitUpload(t, rec, "gone/sub/edit.txt")
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.deletes) != 0 {
		t.Errorf("server deletes %v", rec.deletes)
	}
}

// Folder fileids are recorded (one write per listing, not one per folder), so
// a later server rename of a folder that already existed can be recognised.
func TestReconcileRecordsFolderFileIDsInOneWrite(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	for _, d := range []string{"a", "b", "c"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, d, "x.txt"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("a", true, "ea", "fa"), ph("b", true, "eb", "fb"), ph("c", true, "ec", "fc")}
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()
	w.Reconcile()

	rec.mu.Lock()
	defer rec.mu.Unlock()
	for d, want := range map[string]string{"a": "fa", "b": "fb", "c": "fc"} {
		if rec.fileids[d] != want {
			t.Errorf("fileid[%s] = %q, want %q", d, rec.fileids[d], want)
		}
	}
	if rec.fileidBatches != 1 {
		t.Errorf("%d fileid writes for one listing over two passes, want 1 (only what changed is written)", rec.fileidBatches)
	}
}

// Deck #987: Nextcloud's own activity calls a same-folder move a rename.
func TestMoveKind(t *testing.T) {
	for _, c := range []struct{ from, to, want string }{
		{"a/x.txt", "a/y.txt", "rename"},
		{"x.txt", "y.txt", "rename"},
		{"a/x.txt", "b/x.txt", "move"},
		{"a/x.txt", "x.txt", "move"},
		{"A/x.txt", "a/y.txt", "rename"},
	} {
		if got := moveKind(c.from, c.to); got != c.want {
			t.Errorf("moveKind(%q, %q) = %q, want %q", c.from, c.to, got, c.want)
		}
	}
}
