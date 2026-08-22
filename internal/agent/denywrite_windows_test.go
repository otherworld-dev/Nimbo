//go:build windows

package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// The whole feature rests on this: while the handle is held, another writer must
// be refused, and a reader must not be. If this stops being true the "locked for
// editing" dialog quietly stops appearing.
func TestHoldDenyWriteRefusesWriters(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Budget.xlsx")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	h, err := holdDenyWrite(path)
	if err != nil {
		t.Fatalf("holdDenyWrite: %v", err)
	}

	// A writer — what Word does when opening a document for editing — is refused.
	if f, err := os.OpenFile(path, os.O_RDWR, 0o644); err == nil {
		f.Close()
		h.Close()
		t.Fatal("a second writer succeeded; the document is not actually protected")
	}

	// A reader still works, so the other app can open a read-only copy and read
	// our synthesised owner file to name the holder.
	f, err := os.Open(path)
	if err != nil {
		h.Close()
		t.Fatalf("reader was refused: %v", err)
	}
	f.Close()

	// Releasing restores normal access.
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	f2, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("writer still refused after release: %v", err)
	}
	f2.Close()
}

// Closing twice must not blow up: shutdown and the lock-cleared path can both
// reach the same handle.
func TestHoldDenyWriteCloseIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.docx")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := holdDenyWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Errorf("second close: %v, want nil", err)
	}
}

// A file we cannot hold is not an error worth escalating — most often the local
// user already has it open, in which case it is already protected.
func TestHoldDenyWriteMissingFile(t *testing.T) {
	if _, err := holdDenyWrite(filepath.Join(t.TempDir(), "nope.xlsx")); err == nil {
		t.Error("holding a missing file succeeded; want an error")
	}
}

// We must not stop the user READING a file somebody else has locked — only
// writing it. This is the same assertion as above from the other direction, and
// it is the one that would make the feature intolerable if it regressed.
func TestHoldDenyWriteAllowsConcurrentReaders(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shared.xlsx")
	if err := os.WriteFile(path, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := holdDenyWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile while held: %v", err)
	}
	if string(b) != "payload" {
		t.Errorf("read %q, want payload", b)
	}
}
