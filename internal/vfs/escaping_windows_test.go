//go:build windows

package vfs

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/otherworld/nimbo/internal/cfapi"
)

// escHtaccess is a stand-in for engine.Escaper.Encode covering the one opted-in
// name these tests use. Mirrors the real thing: basename only, suffix appended.
func escHtaccess(rel string) string {
	if path.Base(rel) == ".htaccess" {
		return path.Join(path.Dir(rel), ".htaccess"+".nimboesc")
	}
	return rel
}

func decHtaccess(rel string) string {
	if path.Base(rel) == ".htaccess.nimboesc" {
		return path.Join(path.Dir(rel), ".htaccess")
	}
	return rel
}

// escapingOps wires the encode/decode hooks onto a recorder's Ops.
func escapingOps(r *recorder) Ops {
	o := r.ops()
	o.Encode = escHtaccess
	o.Decode = decHtaccess
	return o
}

// phAt builds a server-listed placeholder whose Identity differs from the local
// name — the shared ph() helper hard-codes Identity == Name and so cannot
// express a disguised file at all.
func phAt(name string, dir bool, etag, fileid string) cfapi.PlaceholderInfo {
	return cfapi.PlaceholderInfo{
		Name: name, IsDir: dir, ModTime: time.Now(), Size: 0,
		Identity: []byte(name), ETag: etag, FileID: fileid,
	}
}

// THE DATA-LOSS CASE. A disguised file lives on disk under its real name
// (.htaccess) and on the server under the escaped one (.htaccess.nimboesc).
// Reconcile joins local and server entries by NAME, so the local file matches
// nothing on the server; because adopt already marked it in-sync it is not
// protected by the NeedsUpload guard, and it gets removed and replaced by a
// second placeholder under the escaped name.
func TestReconcileKeepsADisguisedFileTheServerStoresEscaped(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	local := filepath.Join(root, ".htaccess")
	if err := os.WriteFile(local, []byte("deny from all"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{
		phAt(".htaccess.nimboesc", false, "e-ht", "f-ht"),
	}
	rec.baselines[".htaccess.nimboesc"] = "e-ht" // in sync — nothing to do
	w := bareWatcher(root, escapingOps(rec))
	defer w.cancel()

	w.Reconcile()

	if _, err := os.Stat(local); err != nil {
		t.Fatal("the disguised file was deleted locally: reconcile matched the local name against the escaped server name")
	}
	if _, err := os.Stat(filepath.Join(root, ".htaccess.nimboesc")); err == nil {
		t.Fatal("a second placeholder was created under the escaped name")
	}
}

// The reported bug: a file created inside the mount must be PUT under the name
// the server will accept, not the forbidden raw one.
func TestWriteBackUploadsUnderTheEscapedName(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	local := filepath.Join(root, ".htaccess")
	if err := os.WriteFile(local, []byte("deny from all"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDirty(local)

	rec := newRecorder()
	w := bareWatcher(root, escapingOps(rec))
	defer w.cancel()

	w.handleChange(local)

	if len(rec.uploads) != 1 {
		t.Fatalf("want exactly 1 upload, got %v", rec.uploads)
	}
	if rec.uploads[0] != ".htaccess.nimboesc" {
		t.Fatalf("uploaded as %q, want %q (the server rejects the raw name)", rec.uploads[0], ".htaccess.nimboesc")
	}
}

// The placeholder identity is what the OS hands back on hydration
// (fetchDataCallback -> DownloadRange), so it must name the file as the SERVER
// knows it. A raw identity on a disguised file is a 404 on every open.
func TestWriteBackStampsTheEscapedIdentity(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	local := filepath.Join(root, ".htaccess")
	if err := os.WriteFile(local, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDirty(local)

	rec := newRecorder()
	w := bareWatcher(root, escapingOps(rec))
	defer w.cancel()

	w.handleChange(local)

	got, ok := f.identityOf(local)
	if !ok {
		t.Fatal("file was never marked in-sync")
	}
	if got != ".htaccess.nimboesc" {
		t.Fatalf("placeholder identity = %q, want %q — hydration would 404", got, ".htaccess.nimboesc")
	}
}

// Escaping covers FILE basenames only (engine/escape.go: "v1 escapes file names
// only; directory paths are untouched"). A directory that happens to share an
// opted-in name must still be created under its real name, or its ETag key stops
// matching the parent listing and the subtree-skip that keeps a large mount cheap
// silently dies.
func TestWriteBackNeverEscapesADirectory(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	dir := filepath.Join(root, ".htaccess")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A folder the user just made: PLAIN, not a placeholder. That — not a
	// cleared in-sync bit — is what makes a directory need MKCOL, because a
	// directory placeholder is deliberately left not-in-sync so the shell will
	// ask it to populate.
	f.markPlain(dir)

	rec := newRecorder()
	w := bareWatcher(root, escapingOps(rec))
	defer w.cancel()

	w.handleChange(dir)

	if len(rec.mkdirs) != 1 {
		t.Fatalf("want exactly 1 mkdir, got %v", rec.mkdirs)
	}
	if rec.mkdirs[0] != ".htaccess" {
		t.Fatalf("directory created as %q, want the raw %q", rec.mkdirs[0], ".htaccess")
	}
}

// Deleting a disguised file must DELETE the escaped server name. Sending the raw
// name 404s, orphaning the server copy, and the next reconcile pulls it straight
// back down — a resurrect loop.
func TestWriteBackDeletesTheEscapedName(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	local := filepath.Join(root, ".htaccess")

	rec := newRecorder()
	w := bareWatcher(root, escapingOps(rec))
	defer w.cancel()

	w.handleDelete(local) // already gone from disk

	if len(rec.deletes) != 1 {
		t.Fatalf("want exactly 1 delete, got %v", rec.deletes)
	}
	if rec.deletes[0] != ".htaccess.nimboesc" {
		t.Fatalf("deleted %q, want %q", rec.deletes[0], ".htaccess.nimboesc")
	}
}

// A rename must encode each end independently: renaming notes.txt -> .htaccess
// is a MOVE from the raw name to the escaped one.
func TestWriteBackRenameEncodesBothEndsIndependently(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	oldPath := filepath.Join(root, "notes.txt")
	newPath := filepath.Join(root, ".htaccess")
	if err := os.WriteFile(newPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := newRecorder()
	w := bareWatcher(root, escapingOps(rec))
	defer w.cancel()

	w.handleRename(oldPath, newPath)

	if len(rec.moves) != 1 {
		t.Fatalf("want exactly 1 move, got %v", rec.moves)
	}
	if rec.moves[0] != [2]string{"notes.txt", ".htaccess.nimboesc"} {
		t.Fatalf("moved %v, want [notes.txt .htaccess.nimboesc]", rec.moves[0])
	}
}

// With no encoder wired (escaping off, or a non-Windows-style build), every path
// must pass through untouched — this is the overwhelmingly common case and must
// not regress.
func TestNoEncoderLeavesNamesAlone(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	local := filepath.Join(root, ".htaccess")
	if err := os.WriteFile(local, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDirty(local)

	rec := newRecorder()
	w := bareWatcher(root, rec.ops()) // no Encode/Decode hooks
	defer w.cancel()

	w.handleChange(local)

	if len(rec.uploads) != 1 || rec.uploads[0] != ".htaccess" {
		t.Fatalf("uploads = %v, want [.htaccess] unchanged", rec.uploads)
	}
}

// Junk names are filtered out of the LOCAL side of reconcile (skipName), so they
// never enter localByName. The additions loop must apply the same filter, or a
// junk file present on BOTH sides looks like a missing addition on every single
// pass: CfCreatePlaceholders then fails with ERROR_ALREADY_EXISTS (0x800700b7)
// forever. Observed in the field as a "reconcile create in ...: 0x800700b7
// (processed 3/3)" line on every poll.
func TestReconcileNeverRecreatesSkippedJunk(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	// Present on disk AND listed by the server — the looping case.
	if err := os.WriteFile(filepath.Join(root, ".owncloudsync.log"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{
		ph(".owncloudsync.log", false, "e-log", "f-log"),
		ph("desktop.ini", false, "e-ini", "f-ini"), // junk the server has, we don't
		ph("real.txt", false, "e-real", "f-real"),
	}
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	if _, err := os.Stat(filepath.Join(root, "real.txt")); err != nil {
		t.Error("a genuine server addition was not created")
	}
	if _, err := os.Stat(filepath.Join(root, "desktop.ini")); err == nil {
		t.Error("junk from the server was pulled down as a placeholder")
	}
	if rec.baselines[".owncloudsync.log"] != "" || rec.baselines["desktop.ini"] != "" {
		t.Error("junk was recorded as though it had been created")
	}
}

// Windows filenames are case-insensitive, so a server entry differing only in
// case from the local one IS the same file. Matching it case-sensitively makes
// it look like a missing addition on every pass, and CfCreatePlaceholders then
// fails with ERROR_ALREADY_EXISTS forever. skipName already learned this lesson
// ("a live Desktop.ini once slipped past an exact match").
func TestReconcileMatchesNamesCaseInsensitively(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Photos"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Notes.txt"), []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := newRecorder()
	// The server reports different casing for the same two entries.
	rec.listing[""] = []cfapi.PlaceholderInfo{
		ph("photos", true, "e-p", ""),
		ph("notes.TXT", false, "e-n", "f-n"),
	}
	rec.listing["photos"] = nil
	rec.listing["Photos"] = nil
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	// Assert on what reconcile ASKED to create, not on the filesystem: NTFS is
	// case-insensitive, so a Stat check would pass for the wrong reason.
	if got := f.createdNames(); len(got) != 0 {
		t.Fatalf("reconcile tried to create %v, but those already exist under another case", got)
	}
	// And the existing entries must survive, with their content intact.
	if b, err := os.ReadFile(filepath.Join(root, "Notes.txt")); err != nil || string(b) != "keep me" {
		t.Fatalf("Notes.txt was deleted or clobbered (err=%v, content=%q)", err, string(b))
	}
}

// Live sync blocks a forbidden X when a genuine X.nimboesc is in the same plan
// (engine.FilterBlocked's claimed set): escaping X would PUT over the genuine
// file's server copy. The write-back watcher pushes one file at a time and had
// no such check. Its "plan" is the folder on disk: a neighbour already wearing
// the escaped name means X must not upload. The refusal is reported once, not
// every time the reconcile rescue re-arms the dirty file, and no retry timer
// is set: only the user's rename can clear it (Deck #554, item 1).
func TestWriteBackRefusesAnEscapedNameAGenuineFileOccupies(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	disguised := filepath.Join(root, ".htaccess")
	genuine := filepath.Join(root, ".htaccess.nimboesc")
	for _, p := range []string{disguised, genuine} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.markDirty(disguised)

	rec := newRecorder()
	w := bareWatcher(root, escapingOps(rec))
	defer w.cancel()

	w.handleChange(disguised)
	w.handleChange(disguised) // the reconcile rescue re-arms a dirty file every pass

	if len(rec.uploads) != 0 {
		t.Fatalf("uploaded %v: the genuine .htaccess.nimboesc's server copy would be overwritten", rec.uploads)
	}
	got := rec.reportsOf("upload")
	if len(got) != 1 {
		t.Fatalf("want exactly one upload report for the two refusals, got %+v", got)
	}
	if got[0].path != ".htaccess" || got[0].err == nil || !strings.Contains(got[0].err.Error(), ".htaccess.nimboesc") {
		t.Fatalf("report = %+v, want an error on .htaccess that names .htaccess.nimboesc", got[0])
	}
	if _, ok := f.identityOf(disguised); ok {
		t.Fatal("the refused file was marked in-sync")
	}
	w.mu.Lock()
	_, armed := w.upload[disguised]
	w.mu.Unlock()
	if armed {
		t.Fatal("a retry timer was armed for a refusal only the user can clear")
	}
}

// Once the genuine file is out of the way the disguised one uploads as normal,
// its success clears the sticky error, and a later collision is reported afresh.
func TestWriteBackUploadsOnceTheEscapedNameIsFree(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	disguised := filepath.Join(root, ".htaccess")
	genuine := filepath.Join(root, ".htaccess.nimboesc")
	for _, p := range []string{disguised, genuine} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.markDirty(disguised)

	rec := newRecorder()
	w := bareWatcher(root, escapingOps(rec))
	defer w.cancel()

	w.handleChange(disguised) // refused
	if err := os.Rename(genuine, filepath.Join(root, "config.txt")); err != nil {
		t.Fatal(err)
	}
	w.handleChange(disguised) // free now

	if len(rec.uploads) != 1 || rec.uploads[0] != ".htaccess.nimboesc" {
		t.Fatalf("uploads = %v, want [.htaccess.nimboesc]", rec.uploads)
	}
	got := rec.reportsOf("upload")
	if len(got) != 2 || got[1].path != ".htaccess" || got[1].err != nil {
		t.Fatalf("reports = %+v, want the refusal then a success on .htaccess", got)
	}

	// The name is taken again: a fresh stretch, reported again.
	if err := os.WriteFile(genuine, []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDirty(disguised)
	w.handleChange(disguised)
	if got := rec.reportsOf("upload"); len(got) != 3 || got[2].err == nil {
		t.Fatalf("reports = %+v, want a third, failed report for the new collision", got)
	}
}

// A rename INTO a disguised name is the same collision through the MOVE path:
// notes.txt -> .htaccess would MOVE the server's notes.txt over the genuine
// .htaccess.nimboesc. Refused before the MOVE, reported once, and the move
// bookkeeping is released so reconcile can look again.
func TestWriteBackRenameRefusesAnEscapedNameAGenuineFileOccupies(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	oldPath := filepath.Join(root, "notes.txt")
	newPath := filepath.Join(root, ".htaccess")
	genuine := filepath.Join(root, ".htaccess.nimboesc")
	for _, p := range []string{newPath, genuine} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	rec := newRecorder()
	w := bareWatcher(root, escapingOps(rec))
	defer w.cancel()

	w.handleRename(oldPath, newPath)
	w.handleRename(oldPath, newPath)

	if len(rec.moves) != 0 {
		t.Fatalf("moved %v: the genuine .htaccess.nimboesc's server copy would be overwritten", rec.moves)
	}
	got := rec.reportsOf("move")
	if len(got) != 1 || got[0].path != ".htaccess" || got[0].err == nil || !strings.Contains(got[0].err.Error(), ".htaccess.nimboesc") {
		t.Fatalf("reports = %+v, want one error on .htaccess that names .htaccess.nimboesc", got)
	}
	w.mu.Lock()
	inMove := len(w.inMove)
	w.mu.Unlock()
	if inMove != 0 {
		t.Fatal("the refused move is still marked in flight")
	}
}

// Down-sync reports carried the RAW server name while up-sync reports carry
// the local one, so the activity feed showed ".htaccess.nimboesc" for a pull
// and ".htaccess" for a push of the same file. Worse than cosmetic: the
// recorder keys unresolved errors by path, so an error under one form could
// never be cleared by a success under the other (Deck #554, item 2).
func TestReconcileReportsAPulledDisguisedFileUnderItsLocalName(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()

	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{phAt(".htaccess.nimboesc", false, "e-ht", "f-ht")}
	w := bareWatcher(root, escapingOps(rec))
	defer w.cancel()

	w.Reconcile()

	got := rec.reportsOf("download")
	if len(got) != 1 || got[0].path != ".htaccess" {
		t.Fatalf("download reports = %+v, want one for .htaccess (the local name)", got)
	}
}

// The same for a rename applied from the server side.
func TestReconcileReportsAPulledRenameUnderItsLocalName(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "old.txt"), []byte("hydrated"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := newRecorder()
	rec.fileids["old.txt"] = "fid-1"
	rec.listing[""] = []cfapi.PlaceholderInfo{phAt(".htaccess.nimboesc", false, "e2", "fid-1")}
	w := bareWatcher(root, escapingOps(rec))
	defer w.cancel()

	w.Reconcile()

	got := rec.reportsOf("move")
	if len(got) != 1 || got[0].path != ".htaccess" {
		t.Fatalf("move reports = %+v, want one for .htaccess (the local name)", got)
	}
}

// The once-per-stretch bookkeeping ends when the refused file goes away: a
// file of the same name created later is a new collision and is reported.
func TestWriteBackReportsACollisionAgainAfterTheFileWasDeleted(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	disguised := filepath.Join(root, ".htaccess")
	genuine := filepath.Join(root, ".htaccess.nimboesc")
	for _, p := range []string{disguised, genuine} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.markDirty(disguised)

	rec := newRecorder()
	w := bareWatcher(root, escapingOps(rec))
	defer w.cancel()

	w.handleChange(disguised) // refused
	if err := os.Remove(disguised); err != nil {
		t.Fatal(err)
	}
	w.handleDelete(disguised)
	if err := os.WriteFile(disguised, []byte("again"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDirty(disguised)
	w.handleChange(disguised) // a new file, the same collision

	if got := rec.reportsOf("upload"); len(got) != 2 || got[1].err == nil {
		t.Fatalf("reports = %+v, want the new file's collision reported as well", got)
	}
}

// The same when the refused file is renamed away rather than deleted.
func TestWriteBackReportsACollisionAgainAfterTheFileWasRenamedAway(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	disguised := filepath.Join(root, ".htaccess")
	genuine := filepath.Join(root, ".htaccess.nimboesc")
	for _, p := range []string{disguised, genuine} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.markDirty(disguised)

	rec := newRecorder()
	w := bareWatcher(root, escapingOps(rec))
	defer w.cancel()

	w.handleChange(disguised) // refused
	moved := filepath.Join(root, "moved.txt")
	if err := os.Rename(disguised, moved); err != nil {
		t.Fatal(err)
	}
	w.handleRename(disguised, moved)
	if err := os.WriteFile(disguised, []byte("again"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDirty(disguised)
	w.handleChange(disguised) // a new file, the same collision

	if got := rec.reportsOf("upload"); len(got) != 2 || got[1].err == nil {
		t.Fatalf("reports = %+v, want the new file's collision reported as well", got)
	}
}
