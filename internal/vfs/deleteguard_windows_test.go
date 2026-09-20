package vfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/otherworld/nimbo/internal/cfapi"
	"github.com/otherworld/nimbo/internal/transport"
)

// The delete guard (judgeDelete / keepOnServer).
//
// On an on-demand mount a folder nobody has opened is empty on disk while the
// server holds all of it, so it can vanish locally in one RemoveDirectory, and
// the DELETE the watcher then sends is recursive on the server. These tests pin
// the rule that replaced "gone locally = delete on the server": a vanished path
// is deleted on the server only when every file the DELETE would remove, at any
// depth, was on this computer. Anything else keeps it, and anything unknown
// waits.

func dirEntry(name, identity string) cfapi.PlaceholderInfo {
	return cfapi.PlaceholderInfo{Name: name, IsDir: true, ModTime: time.Now(), Identity: []byte(identity)}
}

func fileEntry(name, identity string) cfapi.PlaceholderInfo {
	return cfapi.PlaceholderInfo{Name: name, Size: 5, ModTime: time.Now(), Identity: []byte(identity), ETag: "e-" + name}
}

func (r *recorder) reportsOf(kind string) []reportRec {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []reportRec
	for _, rr := range r.reports {
		if rr.kind == kind {
			out = append(out, rr)
		}
	}
	return out
}

func (r *recorder) deleteList() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.deletes...)
}

func shortRetries(t *testing.T) {
	t.Helper()
	ob := retryBase
	retryBase = 20 * time.Millisecond
	t.Cleanup(func() { retryBase = ob })
}

// The reporter's hazard, reproduced on the test VM: a folder the shell never
// populated disappears (it was empty on disk) while the server holds files in
// it. Nothing may be deleted on the server; the folder comes back, Explorer is
// told, and the activity feed says so without calling it a failure.
func TestDeleteKeepsAFolderWhoseContentsWereNeverHere(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	rec := newRecorder()
	rec.listing["Lazy"] = []cfapi.PlaceholderInfo{
		fileEntry("a.txt", "Lazy/a.txt"),
		dirEntry("sub", "Lazy/sub"),
	}
	// Reconcile records a baseline for a folder it pulls before anyone opens
	// it: the folder's OWN entry says nothing about its contents.
	rec.baselines["Lazy"] = "e-lazy"
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleDelete(filepath.Join(root, "Lazy")) // already gone from disk

	if d := rec.deleteList(); len(d) != 0 {
		t.Fatalf("a folder whose contents were never here was deleted on the server: %v", d)
	}
	if !strings.Contains(strings.Join(f.createdNames(), ","), "Lazy") {
		t.Fatalf("the folder was not put back; created = %v", f.createdNames())
	}
	if fi, err := os.Stat(filepath.Join(root, "Lazy")); err != nil || !fi.IsDir() {
		t.Fatalf("the folder is not back on disk: %v", err)
	}
	f.mu.Lock()
	notes := append([]string(nil), f.createdNote...)
	f.mu.Unlock()
	if len(notes) != 1 || notes[0] != filepath.Join(root, "Lazy") {
		t.Fatalf("Explorer was not told the folder is back (an open window shows it gone until F5); notes = %v", notes)
	}
	kept := rec.reportsOf("delete-kept")
	if len(kept) != 1 || kept[0].path != "Lazy" || kept[0].err != nil {
		t.Fatalf("want one delete-kept report for Lazy that is not a failure, got %+v", kept)
	}
	if !rec.logged("vfs kept Lazy on the server") {
		t.Fatalf("no log line for the refusal; logs = %v", rec.logLines())
	}
}

// The reviewer's case: one level of files that were here does not vouch for a
// subfolder that was never opened. Every file at every depth has to have been
// here, and a subfolder's own baseline (reconcile pulled it) counts for nothing.
func TestDeleteJudgesTheWholeSubtree(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	rec := newRecorder()
	rec.listing["Photos"] = []cfapi.PlaceholderInfo{
		fileEntry("a.jpg", "Photos/a.jpg"),
		dirEntry("2026", "Photos/2026"),
	}
	rec.listing["Photos/2026"] = []cfapi.PlaceholderInfo{fileEntry("b.jpg", "Photos/2026/b.jpg")}
	rec.baselines["Photos/a.jpg"] = "e-a.jpg"
	rec.baselines["Photos/2026"] = "e-2026"
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleDelete(filepath.Join(root, "Photos"))

	if d := rec.deleteList(); len(d) != 0 {
		t.Fatalf("deleted a folder holding a file two levels down that was never here: %v", d)
	}
	if !rec.logged("never on this computer") {
		t.Fatalf("the refusal does not say why; logs = %v", rec.logLines())
	}
}

// A folder whose every file, at every depth, was here is one the user had: its
// delete goes through.
func TestDeleteSendsAFolderWhoseFilesWereAllHere(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	rec := newRecorder()
	rec.listing["Opened"] = []cfapi.PlaceholderInfo{
		fileEntry("x.txt", "Opened/x.txt"),
		dirEntry("Sub", "Opened/Sub"),
	}
	rec.listing["Opened/Sub"] = []cfapi.PlaceholderInfo{fileEntry("y.txt", "Opened/Sub/y.txt")}
	rec.baselines["Opened/x.txt"] = "e-x.txt"
	rec.baselines["Opened/Sub/y.txt"] = "e-y.txt"
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleDelete(filepath.Join(root, "Opened"))

	if d := rec.deleteList(); len(d) != 1 || d[0] != "Opened" {
		t.Fatalf("a folder the user had must be deleted on the server; deletes = %v", d)
	}
}

// The case in the reporter's log: an EMPTY server folder removed locally is an
// ordinary delete and still reaches the server. So does a tree of empty folders:
// deleting it loses nothing.
func TestDeleteSendsEmptyFoldersToTheServer(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	rec := newRecorder()
	rec.listing["Empty"] = []cfapi.PlaceholderInfo{}
	rec.listing["Tree"] = []cfapi.PlaceholderInfo{dirEntry("B", "Tree/B")}
	rec.listing["Tree/B"] = []cfapi.PlaceholderInfo{}
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleDelete(filepath.Join(root, "Empty"))
	w.handleDelete(filepath.Join(root, "Tree"))

	if d := rec.deleteList(); len(d) != 2 || d[0] != "Empty" || d[1] != "Tree" {
		t.Fatalf("empty folders must be deleted on the server; deletes = %v", d)
	}
	if len(rec.reportsOf("delete-kept")) != 0 {
		t.Fatal("an empty folder was reported as kept")
	}
}

// A file has nothing beneath it: its delete goes straight through.
func TestDeleteOfAFileGoesThrough(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleDelete(filepath.Join(root, "doc.txt"))

	if d := rec.deleteList(); len(d) != 1 || d[0] != "doc.txt" {
		t.Fatalf("deletes = %v", d)
	}
}

// When the server cannot be asked, the delete neither goes out blind nor is
// dropped: it waits and asks again, and goes through once the answer is "empty".
// A timeout is the same as no answer, never a licence to delete.
func TestDeleteWaitsWhenTheServerCannotBeAsked(t *testing.T) {
	for name, cause := range map[string]error{
		"network": errors.New("dial tcp: connection refused"),
		"timeout": fmt.Errorf("PROPFIND Maybe: %w", context.DeadlineExceeded),
	} {
		t.Run(name, func(t *testing.T) {
			shortRetries(t)
			installFakeCf(t)
			root := t.TempDir()
			rec := newRecorder()
			rec.listErr = cause
			w := bareWatcher(root, rec.ops())
			defer w.cancel()

			w.handleDelete(filepath.Join(root, "Maybe"))
			time.Sleep(100 * time.Millisecond) // several retry periods
			if d := rec.deleteList(); len(d) != 0 {
				t.Fatalf("deleted without knowing what the server holds: %v", d)
			}

			rec.mu.Lock()
			rec.listErr = nil
			rec.listing["Maybe"] = []cfapi.PlaceholderInfo{}
			rec.mu.Unlock()
			select {
			case got := <-rec.deleted:
				if got != "Maybe" {
					t.Fatalf("deleted %q", got)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the delete was dropped instead of retried once the server answered")
			}
		})
	}
}

// A path the server no longer has needs no DELETE and must not be retried
// forever: the listing's 404 is final (transport.ErrNotFound, not retryable).
func TestDeleteSkipsAPathTheServerNoLongerHas(t *testing.T) {
	shortRetries(t)
	installFakeCf(t)
	root := t.TempDir()
	rec := newRecorder()
	rec.listErr = fmt.Errorf("path %q not found: %w", "Gone", transport.ErrNotFound)
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleDelete(filepath.Join(root, "Gone"))
	time.Sleep(150 * time.Millisecond) // several retry periods

	if d := rec.deleteList(); len(d) != 0 {
		t.Fatalf("sent a DELETE for a path the server does not have: %v", d)
	}
	rec.mu.Lock()
	calls := rec.listCalls["Gone"]
	rec.mu.Unlock()
	if calls != 1 {
		t.Fatalf("the server was asked %d times about a path it does not have; want 1", calls)
	}
}

// A folder's delete waits for its contents' own deletes (a folder DELETE that
// races its children's came back 423 Locked on the test VM and was dropped).
func TestDeleteOfAFolderWaitsForItsContentsDeletes(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	rec := newRecorder()
	rec.listing["P"] = []cfapi.PlaceholderInfo{}
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.scheduleDelete(filepath.Join(root, "P", "c.txt")) // the child's own delete, pending
	w.handleDelete(filepath.Join(root, "P"))

	if d := rec.deleteList(); len(d) != 0 {
		t.Fatalf("the folder's DELETE went out while its contents' was still pending: %v", d)
	}
	var order []string
	for len(order) < 2 {
		select {
		case got := <-rec.deleted:
			order = append(order, got)
		case <-time.After(6 * time.Second):
			t.Fatalf("deletes never completed; got %v", order)
		}
	}
	if order[0] != "P/c.txt" || order[1] != "P" {
		t.Fatalf("want the child deleted before its folder, got %v", order)
	}
}

// A tool that removes empty folders bottom-up removes the parent right after
// the child the guard kept: deleting the parent would take the kept child, so
// the parent is kept too. P's listing is deliberately empty here to isolate
// that rule; with a real listing its own walk would find the file as well.
func TestDeleteKeepsAFolderWhenSomethingInsideWasKept(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	rec := newRecorder()
	rec.listing["P/Lazy"] = []cfapi.PlaceholderInfo{fileEntry("a.txt", "P/Lazy/a.txt")}
	rec.listing["P"] = []cfapi.PlaceholderInfo{}
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleDelete(filepath.Join(root, "P", "Lazy"))
	w.handleDelete(filepath.Join(root, "P"))

	if d := rec.deleteList(); len(d) != 0 {
		t.Fatalf("deleted on the server: %v", d)
	}
	if !rec.logged("something inside it was kept") {
		t.Fatalf("the parent's refusal does not say why; logs = %v", rec.logLines())
	}
	// The child could not be put back (its parent was gone at the time): the
	// next pass must sweep everything so it returns.
	if !w.lostEvents.Load() {
		t.Fatal("a kept folder that could not be put back did not ask for a full sweep")
	}
}

// A tree too large to check is kept rather than deleted unchecked.
func TestDeleteKeepsATreeTooLargeToCheck(t *testing.T) {
	om := maxDeleteCheckListings
	maxDeleteCheckListings = 3
	t.Cleanup(func() { maxDeleteCheckListings = om })
	installFakeCf(t)
	root := t.TempDir()
	rec := newRecorder()
	var kids []cfapi.PlaceholderInfo
	for i := 1; i <= 4; i++ {
		n := fmt.Sprintf("d%d", i)
		kids = append(kids, dirEntry(n, "Big/"+n))
		rec.listing["Big/"+n] = []cfapi.PlaceholderInfo{}
	}
	rec.listing["Big"] = kids
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleDelete(filepath.Join(root, "Big"))

	if d := rec.deleteList(); len(d) != 0 {
		t.Fatalf("deleted a tree it never finished checking: %v", d)
	}
	if !rec.logged("too many to check") {
		t.Fatalf("logs = %v", rec.logLines())
	}
}

// An old baseline is not proof: the folder was deleted and re-created on the
// server since this computer listed it, and the file of the same name is a new
// version (a new ETag) that was never here.
func TestDeleteKeepsWhenABaselineIsOutOfDate(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	rec := newRecorder()
	rec.listing["Reports"] = []cfapi.PlaceholderInfo{fileEntry("q1.xlsx", "Reports/q1.xlsx")} // ETag e-q1.xlsx
	rec.baselines["Reports/q1.xlsx"] = "e-old"
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleDelete(filepath.Join(root, "Reports"))

	if d := rec.deleteList(); len(d) != 0 {
		t.Fatalf("an out-of-date baseline vouched for a file never here: %v", d)
	}
}

// Population never shows an end-to-end encrypted folder, so a folder holding
// only one looks empty even after it is opened. Its contents were never here.
func TestDeleteKeepsAFolderHoldingAnEncryptedFolder(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	rec := newRecorder()
	rec.checkStrict = true
	vault := dirEntry("Vault", "Documents/Vault")
	vault.Encrypted = true
	rec.listing["Documents"] = []cfapi.PlaceholderInfo{vault}
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleDelete(filepath.Join(root, "Documents"))

	if d := rec.deleteList(); len(d) != 0 {
		t.Fatalf("deleted a folder holding an encrypted folder: %v", d)
	}
	if !rec.logged("end-to-end encrypted") {
		t.Fatalf("logs = %v", rec.logLines())
	}
}

// Under a non-empty remote root the guard must list the path the server knows
// (relative to the sync root, not the full server path). The strict listing
// answers "not found" for anything else, which would silently drop the delete.
func TestDeleteGuardListsTheRightPathUnderARemoteRoot(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	rec := newRecorder()
	rec.checkStrict = true
	rec.listing["F"] = []cfapi.PlaceholderInfo{}
	w := bareWatcher(root, rec.ops())
	w.remoteRoot = "Sub"
	defer w.cancel()

	w.handleDelete(filepath.Join(root, "F"))

	if d := rec.deleteList(); len(d) != 1 || d[0] != "Sub/F" {
		t.Fatalf("deletes = %v, want [Sub/F]", d)
	}
}

// The walk can take a while. If the path is back on disk by the time it
// finishes (re-created, uploaded again), the DELETE must not go out.
func TestDeleteRechecksThePathAfterTheWalk(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	rec := newRecorder()
	ops := rec.ops()
	ops.CheckList = func(rel string) ([]cfapi.PlaceholderInfo, error) {
		// The user's editor writes the file again while the guard is asking.
		_ = os.WriteFile(filepath.Join(root, "a.txt"), []byte("new"), 0o644)
		return []cfapi.PlaceholderInfo{}, nil
	}
	w := bareWatcher(root, ops)
	defer w.cancel()

	w.handleDelete(filepath.Join(root, "a.txt"))

	if d := rec.deleteList(); len(d) != 0 {
		t.Fatalf("deleted a path that was back on disk: %v", d)
	}
}

// Shutting down in the middle of the walk sends nothing.
func TestDeleteSendsNothingWhenStoppedDuringTheWalk(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	rec := newRecorder()
	ops := rec.ops()
	var w *Watcher
	ops.CheckList = func(rel string) ([]cfapi.PlaceholderInfo, error) {
		w.cancel()
		return []cfapi.PlaceholderInfo{}, nil
	}
	w = bareWatcher(root, ops)

	w.handleDelete(filepath.Join(root, "a.txt"))

	if d := rec.deleteList(); len(d) != 0 {
		t.Fatalf("deleted during shutdown: %v", d)
	}
}

// A MOVE onto the path (or a folder above it) that has not landed yet means the
// server cannot answer what a delete would remove: wait for it.
func TestDeleteWaitsForAMoveInFlight(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	rec := newRecorder()
	rec.listing["P/a.txt"] = []cfapi.PlaceholderInfo{}
	w := bareWatcher(root, rec.ops())
	defer w.cancel()
	w.mu.Lock()
	w.inMove[strings.ToLower(filepath.Join(root, "P"))] = true
	w.mu.Unlock()

	w.handleDelete(filepath.Join(root, "P", "a.txt"))
	if d := rec.deleteList(); len(d) != 0 {
		t.Fatalf("deleted while a move onto its folder was in flight: %v", d)
	}

	w.mu.Lock()
	delete(w.inMove, strings.ToLower(filepath.Join(root, "P")))
	w.mu.Unlock()
	select {
	case got := <-rec.deleted:
		if got != "P/a.txt" {
			t.Fatalf("deleted %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the delete was dropped instead of waiting for the move")
	}
}

// Reconcile must not pull back a folder whose delete is still being checked:
// the delete would then find it "came back" and drop it.
func TestReconcileLeavesAPendingDeleteAlone(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("P", true, "etag-p", "")}
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.scheduleDelete(filepath.Join(root, "P")) // gone here, delete pending
	w.Reconcile()

	for _, n := range f.createdNames() {
		if n == "P" {
			t.Fatal("reconcile pulled back a folder whose delete was pending")
		}
	}
}
