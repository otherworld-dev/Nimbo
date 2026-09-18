//go:build windows

package cfapi

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestRenameClearsInSyncButNotModifiedData pins the measurement the whole
// dirty predicate rests on (2026-09-15, Windows 10.0.26200):
//
//	A clean online-only stub    : InSyncState 1, ModifiedDataSize 0
//	…moved by another process   : InSyncState 0, ModifiedDataSize 0
//	…renamed by another process : InSyncState 0, ModifiedDataSize 0
//	A hydrated file, clean      : InSyncState 1, ModifiedDataSize 0
//	…written locally            : InSyncState 0, ModifiedDataSize 4096
//	…then moved                 : InSyncState 0, ModifiedDataSize 4096
//	…then MarkInSync'd          : InSyncState 1, ModifiedDataSize 0
//	A plain file                : not a placeholder (never uploaded)
//	…converted by MarkInSync    : InSyncState 1, ModifiedDataSize 0
//
// i.e. the cloud filter clears the in-sync bit for a rename as readily as for
// an edit, so the bit alone cannot tell the two apart — the modified data can,
// and is unmoved by the rename. Everything the move path does with a "dirty"
// file hangs on this distinction: treating a moved clean stub as dirty
// scheduled an upload that parked the just-moved server copy as a conflicted
// copy (seen on the VM, 2026-09-15).
//
// Since fix wave 3 that distinction is Inspect's too, which is why the moved
// and renamed stubs below expect NeedsUpload FALSE — the three lines that were
// `true` here while the in-sync bit was still the verdict. The last two rows
// pin the CLEAN direction: MarkInSync clears the unsynced content for both
// kinds of never-uploaded bytes, so an uploaded file does not read as forever
// dirty.
// Live-driver test, opt in with NIMBO_CFAPI_LIVE=1.
func TestRenameClearsInSyncButNotModifiedData(t *testing.T) {
	if os.Getenv("NIMBO_CFAPI_LIVE") == "" {
		t.Skip("set NIMBO_CFAPI_LIVE=1 to run against the real Cloud Files API")
	}
	if !Supported() {
		t.Skip("Cloud Files API not available")
	}
	root := filepath.Join(t.TempDir(), "modroot")
	for _, d := range []string{root, filepath.Join(root, "dst")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = Purge(root) })

	hydrate := func(identity []byte, offset, length int64) ([]byte, error) {
		return make([]byte, length), nil
	}
	list := func(rel string) []PlaceholderInfo { return []PlaceholderInfo{} }
	connKey, err := Mount(root, "NimboModifiedTest", "", hydrate, list)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	defer Unmount(root, connKey)

	if err := CreatePlaceholders(root, []PlaceholderInfo{
		{Name: "clean.bin", Size: 4096, ModTime: time.Now(), Identity: []byte("remote/clean.bin")},
		{Name: "edited.bin", Size: 4096, ModTime: time.Now(), Identity: []byte("remote/edited.bin")},
	}); err != nil {
		t.Fatalf("CreatePlaceholders: %v", err)
	}

	// Renames must come from ANOTHER process for the filter to treat them as
	// a user's rename at all (see TestRenameCompletionReportsMoves).
	mv := func(src, dst string) {
		t.Helper()
		out, merr := exec.Command("cmd.exe", "/c", "move", "/Y", src, dst).CombinedOutput()
		if merr != nil {
			t.Fatalf("move %q -> %q: %v: %s", src, dst, merr, out)
		}
		time.Sleep(200 * time.Millisecond)
	}
	check := func(label, path string, wantNeedsUpload, wantModified bool) {
		t.Helper()
		ch, ierr := Inspect(path)
		if ierr != nil {
			t.Fatalf("%s: Inspect: %v", label, ierr)
		}
		mod, merr := PlaceholderModified(path)
		if merr != nil {
			t.Fatalf("%s: PlaceholderModified: %v", label, merr)
		}
		t.Logf("%-26s Inspect.NeedsUpload=%v PlaceholderModified=%v", label, ch.NeedsUpload, mod)
		if ch.NeedsUpload != wantNeedsUpload {
			t.Errorf("%s: Inspect.NeedsUpload = %v, want %v", label, ch.NeedsUpload, wantNeedsUpload)
		}
		if mod != wantModified {
			t.Errorf("%s: PlaceholderModified = %v, want %v", label, mod, wantModified)
		}
	}

	// A clean online-only stub, before and after a move and a rename made by
	// another process. The in-sync bit goes; the modified data does not appear.
	clean := filepath.Join(root, "clean.bin")
	check("clean stub", clean, false, false)
	moved := filepath.Join(root, "dst", "clean.bin")
	mv(clean, moved)
	check("clean stub, moved", moved, false, false)
	renamed := filepath.Join(root, "dst", "clean2.bin")
	mv(moved, renamed)
	check("clean stub, renamed", renamed, false, false)
	if attrs, _, aerr := findAttrTag(renamed); aerr != nil {
		t.Fatalf("findAttrTag: %v", aerr)
	} else if attrs&fileAttrRecallOnDataAccess == 0 {
		t.Error("the moved stub is no longer online-only — something hydrated it")
	}

	// A hydrated file written locally: the modified data is what a real
	// pending edit looks like, and it survives the file's own move.
	edited := filepath.Join(root, "edited.bin")
	if _, rerr := os.ReadFile(edited); rerr != nil { // hydrates it
		t.Fatalf("read edited.bin: %v", rerr)
	}
	time.Sleep(200 * time.Millisecond)
	check("hydrated, clean", edited, false, false)
	f, oerr := os.OpenFile(edited, os.O_WRONLY, 0o644)
	if oerr != nil {
		t.Fatalf("open edited.bin: %v", oerr)
	}
	if _, werr := f.WriteAt([]byte("LOCAL EDIT"), 0); werr != nil {
		f.Close()
		t.Fatalf("write edited.bin: %v", werr)
	}
	if cerr := f.Close(); cerr != nil {
		t.Fatalf("close edited.bin: %v", cerr)
	}
	time.Sleep(200 * time.Millisecond)
	check("hydrated, edited", edited, true, true)
	editedMoved := filepath.Join(root, "dst", "edited.bin")
	mv(edited, editedMoved)
	check("hydrated, edited, moved", editedMoved, true, true)

	// …and MarkInSync on that edited file clears the unsynced content: the
	// upload happened, so there is nothing local the server has not got. This
	// is the predicate's CLEAN direction — if MarkInSync left ModifiedDataSize
	// standing, every uploaded file would read as forever dirty.
	if err := MarkInSync(editedMoved, []byte("remote/dst/edited.bin")); err != nil {
		t.Fatalf("MarkInSync(edited): %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	check("hydrated, edited, marked", editedMoved, false, false)

	// A plain file is never-uploaded content: modified by definition.
	plain := filepath.Join(root, "plain.bin")
	if werr := os.WriteFile(plain, []byte("hello"), 0o644); werr != nil {
		t.Fatalf("write plain.bin: %v", werr)
	}
	if _, perr := PlaceholderIdentity(plain); perr != ErrNotPlaceholder {
		t.Errorf("PlaceholderIdentity(plain) = %v, want %v", perr, ErrNotPlaceholder)
	}
	mod, merr := PlaceholderModified(plain)
	if merr != nil {
		t.Fatalf("PlaceholderModified(plain): %v", merr)
	}
	if !mod {
		t.Error("a plain file reads as unmodified — its content has never been uploaded")
	}
	// …and converting it in place with MarkInSync clears that too: the same
	// CLEAN direction for the other kind of never-uploaded content.
	if err := MarkInSync(plain, []byte("remote/plain.bin")); err != nil {
		t.Fatalf("MarkInSync(plain): %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	check("plain file, marked", plain, false, false)
}
