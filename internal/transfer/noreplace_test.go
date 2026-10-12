package transfer

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// renameNoReplace is what stands between a rename and a file it would
// replace: the conflicted-copy names (Deck #924) and server-side moves
// (Deck #947) both rely on it refusing, and on nothing being touched when it
// does.
func TestRenameNoReplace(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	a, b := write("a.txt", "A"), write("b.txt", "B")

	err := renameNoReplace(a, b)
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("renaming onto an existing file: err = %v, want os.ErrExist", err)
	}
	if got, _ := os.ReadFile(b); string(got) != "B" {
		t.Fatalf("the existing file was changed: %q", got)
	}
	if got, _ := os.ReadFile(a); string(got) != "A" {
		t.Fatalf("the file being renamed was lost: %q", got)
	}

	c := filepath.Join(dir, "c.txt")
	if err := renameNoReplace(a, c); err != nil {
		t.Fatalf("renaming onto a free name: %v", err)
	}
	if got, _ := os.ReadFile(c); string(got) != "A" {
		t.Fatalf("content after the rename = %q", got)
	}

	// Folders too: the file/folder type conflict sets a folder aside.
	d1, d2 := filepath.Join(dir, "d1"), filepath.Join(dir, "d2")
	if err := os.MkdirAll(filepath.Join(d1, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(d2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := renameNoReplace(d1, d2); !errors.Is(err, os.ErrExist) {
		t.Fatalf("renaming a folder onto an existing folder: err = %v, want os.ErrExist", err)
	}
	d3 := filepath.Join(dir, "d3")
	if err := renameNoReplace(d1, d3); err != nil {
		t.Fatalf("renaming a folder onto a free name: %v", err)
	}
	if _, err := os.Stat(filepath.Join(d3, "sub")); err != nil {
		t.Fatalf("the folder's contents did not move with it: %v", err)
	}

	// The fallback behaves the same way.
	e, f := write("e.txt", "E"), write("f.txt", "F")
	if err := renameNoReplaceFallback(e, f); !errors.Is(err, os.ErrExist) {
		t.Fatalf("fallback onto an existing file: err = %v, want os.ErrExist", err)
	}
	if err := renameNoReplaceFallback(e, filepath.Join(dir, "g.txt")); err != nil {
		t.Fatalf("fallback onto a free name: %v", err)
	}
}
