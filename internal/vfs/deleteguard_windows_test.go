package vfs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/otherworld/nimbo/internal/cfapi"
	"github.com/otherworld/nimbo/internal/transport"
)

// The delete guard (unseenContents / keepUnseen).
//
// On an on-demand mount a folder nobody has opened is empty on disk while the
// server holds all of it, so it can vanish locally in one RemoveDirectory, and
// the DELETE the watcher then sends is recursive on the server. These tests pin
// the rule that replaced "gone locally = delete on the server": a vanished path
// is deleted on the server only when the server holds nothing under it, or when
// this computer once listed what it held.

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

func (r *recorder) deleteCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.deletes)
}

func (r *recorder) listCount(rel string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.listCalls[rel]
}

// The reporter's hazard: a folder the shell never populated disappears (it was
// empty on disk), while the server holds files under it. Nothing may be deleted
// on the server, the folder comes back as a placeholder, and the user is told.
func TestDeleteKeepsAFolderWhoseContentsWereNeverHere(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	rec := newRecorder()
	rec.listing["Lazy"] = []cfapi.PlaceholderInfo{
		fileEntry("a.txt", "Lazy/a.txt"),
		dirEntry("sub", "Lazy/sub"),
	}
	rec.listing[""] = []cfapi.PlaceholderInfo{dirEntry("Lazy", "Lazy")}
	// Reconcile records a baseline for a folder it pulls before anyone opens
	// it: the folder's OWN entry must not count as having seen its contents.
	rec.baselines["Lazy"] = "e-lazy"
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleDelete(filepath.Join(root, "Lazy")) // already gone from disk

	if n := rec.deleteCount(); n != 0 {
		t.Fatalf("a folder whose contents were never here was deleted on the server: %v", rec.deletes)
	}
	if !strings.Contains(strings.Join(f.createdNames(), ","), "Lazy") {
		t.Fatalf("the folder was not put back; created = %v", f.createdNames())
	}
	if fi, err := os.Stat(filepath.Join(root, "Lazy")); err != nil || !fi.IsDir() {
		t.Fatalf("the folder is not back on disk: %v", err)
	}
	kept := rec.reportsOf("delete-kept")
	if len(kept) != 1 || kept[0].path != "Lazy" || kept[0].err == nil {
		t.Fatalf("want one delete-kept report for Lazy carrying the explanation, got %+v", kept)
	}
	if !rec.logged("vfs kept Lazy on the server") {
		t.Fatalf("no log line for the refusal; logs = %v", rec.logLines())
	}
}

// The case in the reporter's log (YellowPages): an EMPTY server folder removed
// locally is an ordinary delete and still reaches the server.
func TestDeleteSendsAnEmptyFolderToTheServer(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	rec := newRecorder()
	rec.listing["Empty"] = []cfapi.PlaceholderInfo{}
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleDelete(filepath.Join(root, "Empty"))

	if rec.deleteCount() != 1 || rec.deletes[0] != "Empty" {
		t.Fatalf("an empty folder's delete must reach the server; deletes = %v", rec.deletes)
	}
	if len(rec.reportsOf("delete-kept")) != 0 {
		t.Fatal("an empty folder was reported as kept")
	}
}

// A folder whose contents were listed here (a baseline beneath it) is one the
// user had: its delete goes through, without even asking the server first.
func TestDeleteSendsAFolderWhoseContentsWereListedHere(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	rec := newRecorder()
	rec.baselines["Opened/x.txt"] = "e-x"
	rec.listing["Opened"] = []cfapi.PlaceholderInfo{fileEntry("x.txt", "Opened/x.txt")}
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleDelete(filepath.Join(root, "Opened"))

	if rec.deleteCount() != 1 || rec.deletes[0] != "Opened" {
		t.Fatalf("a folder the user had must be deleted on the server; deletes = %v", rec.deletes)
	}
	if n := rec.listCount("Opened"); n != 0 {
		t.Fatalf("the guard asked the server about a folder it already knew (%d listings)", n)
	}
}

// A sibling's baseline must not vouch for a folder whose name merely starts
// the same way ("Lazy2/..." is not beneath "Lazy").
func TestDeleteGuardIsNotFooledByASiblingWithTheSamePrefix(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	rec := newRecorder()
	rec.baselines["Lazy2/x.txt"] = "e-x"
	rec.listing["Lazy"] = []cfapi.PlaceholderInfo{fileEntry("a.txt", "Lazy/a.txt")}
	rec.listing[""] = []cfapi.PlaceholderInfo{dirEntry("Lazy", "Lazy")}
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleDelete(filepath.Join(root, "Lazy"))

	if n := rec.deleteCount(); n != 0 {
		t.Fatalf("deleted on the strength of a sibling's baseline: %v", rec.deletes)
	}
}

// A mirrored file (its server file id is known) needs no listing: nothing lives
// beneath a file, so the delete goes straight through.
func TestDeleteOfAMirroredFileSkipsTheListing(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	rec := newRecorder()
	rec.fileids["doc.txt"] = "123"
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleDelete(filepath.Join(root, "doc.txt"))

	if rec.deleteCount() != 1 || rec.deletes[0] != "doc.txt" {
		t.Fatalf("deletes = %v", rec.deletes)
	}
	if n := rec.listCount("doc.txt"); n != 0 {
		t.Fatalf("listed a mirrored file %d time(s) before deleting it", n)
	}
}

// When the server cannot be asked, the delete neither goes out blind nor is
// dropped: it waits and asks again, and goes through once the answer is "empty".
func TestDeleteWaitsWhenTheServerCannotBeAsked(t *testing.T) {
	ob := retryBase
	retryBase = 20 * time.Millisecond
	t.Cleanup(func() { retryBase = ob })
	installFakeCf(t)
	root := t.TempDir()
	rec := newRecorder()
	rec.listErr = errors.New("dial tcp: connection refused")
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleDelete(filepath.Join(root, "Maybe"))
	if n := rec.deleteCount(); n != 0 {
		t.Fatalf("deleted without knowing what the server holds: %v", rec.deletes)
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
}

// A listing that fails for good (the server no longer has the path) leaves
// the delete as it always was: harmless, and not retried forever.
func TestDeleteProceedsWhenTheServerNoLongerHasThePath(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	rec := newRecorder()
	rec.listErr = &transport.StatusError{Op: "PROPFIND", Path: "Gone", Code: 404, Status: "404 Not Found"}
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleDelete(filepath.Join(root, "Gone"))

	if rec.deleteCount() != 1 || rec.deletes[0] != "Gone" {
		t.Fatalf("deletes = %v", rec.deletes)
	}
}

// A whole lazy subtree removed at once: the parent is gone too, so the child
// cannot be put back yet, but it must still not be deleted on the server.
func TestDeleteKeepsTheFolderEvenWhenItCannotBePutBack(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	rec := newRecorder()
	rec.listing["Gone/Lazy"] = []cfapi.PlaceholderInfo{fileEntry("a.txt", "Gone/Lazy/a.txt")}
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleDelete(filepath.Join(root, "Gone", "Lazy"))

	if n := rec.deleteCount(); n != 0 {
		t.Fatalf("deleted on the server: %v", rec.deletes)
	}
	if !rec.logged("could not be put back here yet") {
		t.Fatalf("the refusal does not say the folder was not restored; logs = %v", rec.logLines())
	}
}
