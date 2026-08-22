package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/otherworld/nimbo/internal/config"
	"github.com/otherworld/nimbo/internal/officelock"
)

func newTestWarner(t *testing.T) (*lockWarner, config.Dirs, string) {
	t.Helper()
	d := config.Dirs{Config: t.TempDir(), Data: t.TempDir()}
	return newLockWarner(d, func() string { return "Bob Smith" }), d, t.TempDir()
}

func writeDoc(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// Warning a locked file drops the name carriers beside it, and clearing removes
// exactly those again.
func TestWarnCreatesAndClearsNameCarriers(t *testing.T) {
	w, _, dir := newTestWarner(t)
	doc := writeDoc(t, dir, "Budget.xlsx")

	w.apply([]LockedFile{{Path: "Budget.xlsx", Abs: doc, Owner: "bob", OwnerDisplay: "Bob Smith"}})

	owner := filepath.Join(dir, officelock.OwnerFile("Budget.xlsx"))
	libre := filepath.Join(dir, officelock.LibreLockFile("Budget.xlsx"))
	if _, err := os.Stat(owner); err != nil {
		t.Errorf("owner file not written: %v", err)
	}
	if _, err := os.Stat(libre); err != nil {
		t.Errorf("LibreOffice lock file not written: %v", err)
	}
	// The owner file must name the holder — that is its entire purpose.
	if b, err := os.ReadFile(owner); err == nil {
		if got, ok := officelock.ParseOwnerFileUser(b); !ok || got != "Bob Smith" {
			t.Errorf("owner file names %q (%v), want Bob Smith", got, ok)
		}
	}

	w.apply(nil) // lock released
	if _, err := os.Stat(owner); !os.IsNotExist(err) {
		t.Errorf("owner file survived the clear: %v", err)
	}
	if _, err := os.Stat(libre); !os.IsNotExist(err) {
		t.Errorf("LibreOffice lock file survived the clear: %v", err)
	}
}

// The dangerous case: a REAL Office session's owner file must never be
// overwritten, and must never be deleted when our warning clears. Doing either
// would break somebody's genuine Word session.
func TestWarnLeavesRealOwnerFilesAlone(t *testing.T) {
	w, _, dir := newTestWarner(t)
	doc := writeDoc(t, dir, "Report.docx")

	real := filepath.Join(dir, officelock.OwnerFile("Report.docx"))
	if err := os.WriteFile(real, []byte("REAL WORD FILE"), 0o644); err != nil {
		t.Fatal(err)
	}

	w.apply([]LockedFile{{Path: "Report.docx", Abs: doc, OwnerDisplay: "Bob Smith"}})
	b, err := os.ReadFile(real)
	if err != nil || string(b) != "REAL WORD FILE" {
		t.Fatalf("existing owner file was overwritten: %q, %v", b, err)
	}

	w.apply(nil)
	if b, err := os.ReadFile(real); err != nil || string(b) != "REAL WORD FILE" {
		t.Fatalf("existing owner file was deleted or altered: %q, %v", b, err)
	}
}

// A file we never created must not be swept, even if it looks exactly like one
// of ours.
func TestSweepOnlyRemovesOurOwnFiles(t *testing.T) {
	w, d, dir := newTestWarner(t)
	ours := filepath.Join(dir, "~$ours.xlsx")
	theirs := filepath.Join(dir, "~$theirs.xlsx")
	for _, p := range []string{ours, theirs} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Only `ours` is recorded, as if a previous run had made it.
	if err := d.SaveSynthFiles([]string{ours}); err != nil {
		t.Fatal(err)
	}

	if n := w.sweep(); n != 1 {
		t.Errorf("sweep removed %d, want 1", n)
	}
	if _, err := os.Stat(ours); !os.IsNotExist(err) {
		t.Error("our own leftover was not removed")
	}
	if _, err := os.Stat(theirs); err != nil {
		t.Error("an unrecorded owner file was removed; that would break a real Office session")
	}
	if got := d.LoadSynthFiles(); len(got) != 0 {
		t.Errorf("record not cleared after sweep: %v", got)
	}
}

// The record is written before the file, so a crash between the two still
// leaves something the next sweep can tidy.
func TestSynthFilesAreRecorded(t *testing.T) {
	w, d, dir := newTestWarner(t)
	doc := writeDoc(t, dir, "Budget.xlsx")

	w.apply([]LockedFile{{Path: "Budget.xlsx", Abs: doc, OwnerDisplay: "Bob Smith"}})
	rec := d.LoadSynthFiles()
	if len(rec) != 2 {
		t.Fatalf("recorded %v, want both name carriers", rec)
	}

	w.apply(nil)
	if rec := d.LoadSynthFiles(); len(rec) != 0 {
		t.Errorf("record still holds %v after clearing", rec)
	}
}

// A document that isn't downloaded here can't be warned about, and must not
// leave stray files behind.
func TestWarnSkipsMissingDocuments(t *testing.T) {
	w, d, dir := newTestWarner(t)
	w.apply([]LockedFile{{Path: "ghost.xlsx", Abs: filepath.Join(dir, "ghost.xlsx")}})

	if got := d.LoadSynthFiles(); len(got) != 0 {
		t.Errorf("created %v for a document that is not here", got)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("directory is not empty: %v", ents)
	}
}

// Re-applying the same lock must not duplicate work or re-toast the filesystem.
func TestApplyIsIdempotent(t *testing.T) {
	w, d, dir := newTestWarner(t)
	doc := writeDoc(t, dir, "Budget.xlsx")
	f := []LockedFile{{Path: "Budget.xlsx", Abs: doc, OwnerDisplay: "Bob Smith"}}

	// Release before the test ends: the handle denies DELETE as well as write,
	// so leaving it open defeats t.TempDir's cleanup — which is itself a decent
	// demonstration that the handle works.
	defer w.closeAll()

	w.apply(f)
	first := d.LoadSynthFiles()
	w.apply(f)
	if got := d.LoadSynthFiles(); len(got) != len(first) {
		t.Errorf("second apply changed the record: %v -> %v", first, got)
	}
	if len(w.held) != 1 {
		t.Errorf("held = %d entries, want 1", len(w.held))
	}
}

// closeAll is the shutdown path: nothing of ours may be left on disk.
func TestCloseAllRemovesEverything(t *testing.T) {
	w, d, dir := newTestWarner(t)
	doc := writeDoc(t, dir, "Budget.xlsx")
	w.apply([]LockedFile{{Path: "Budget.xlsx", Abs: doc, OwnerDisplay: "Bob Smith"}})

	w.closeAll()
	if got := d.LoadSynthFiles(); len(got) != 0 {
		t.Errorf("record still holds %v", got)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 { // just the document
		t.Errorf("left files behind: %v", ents)
	}
}
