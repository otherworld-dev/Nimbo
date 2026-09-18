//go:build windows

package vfs

// Deck #557 for on-demand mode: a folder shared with the user that is unshared
// vanishes from the listing. Reconcile used to read that as a deletion and
// RemoveAll the placeholder tree — including files the user had downloaded or
// edited. Now a vanished item the listing once marked as a share/mount ROOT is
// SALVAGED (hydrated placeholders reverted to plain files, online-only stubs
// removed, since they hold no bytes) and handed to Ops.Detached, which parks
// the copy outside the cloud folder. Nothing is deleted on the server.

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/otherworld/nimbo/internal/cfapi"
)

// unshareWorld builds a root holding the user's own file plus a share "Team"
// (marked as a mount root) with a hydrated file, an online-only stub, and a
// subfolder with a hydrated file — and a server listing that no longer has it.
func unshareWorld(t *testing.T) (*fakeCf, *recorder, *Watcher, string) {
	t.Helper()
	f := installFakeCf(t)
	root := t.TempDir()
	team := filepath.Join(root, "Team")
	if err := os.MkdirAll(filepath.Join(team, "old"), 0o755); err != nil {
		t.Fatal(err)
	}
	for rel, body := range map[string]string{"mine.txt": "mine", "Team/Budget.xlsx": "numbers", "Team/big.bin": "", "Team/old/n.txt": "n"} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.markDehydrated(filepath.Join(team, "big.bin"))
	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("mine.txt", false, "e-mine", "f-mine")}
	rec.baselines["mine.txt"] = "e-mine"
	rec.mountRoots = map[string]bool{"Team": true}
	w := bareWatcher(root, rec.ops())
	t.Cleanup(w.cancel)
	return f, rec, w, team
}

func TestReconcileSalvagesAndParksAnUnsharedFolder(t *testing.T) {
	f, rec, w, team := unshareWorld(t)

	w.Reconcile()

	if got := rec.detachedCalls(); len(got) != 1 || got[0] != [2]string{team, "Team"} {
		t.Fatalf("Ops.Detached calls = %v, want [[%s Team]]", got, team)
	}
	for _, rel := range []string{"Budget.xlsx", "old/n.txt"} {
		if _, err := os.Stat(filepath.Join(team, filepath.FromSlash(rel))); err != nil {
			t.Errorf("hydrated file lost: %s", rel)
		}
	}
	if _, err := os.Stat(filepath.Join(team, "big.bin")); err == nil {
		t.Error("online-only stub kept — it holds no bytes and can never hydrate again")
	}
	reverted := f.revertedPaths()
	sort.Strings(reverted)
	want := []string{team, filepath.Join(team, "Budget.xlsx"), filepath.Join(team, "old"), filepath.Join(team, "old", "n.txt")}
	sort.Strings(want)
	if strings.Join(reverted, "|") != strings.Join(want, "|") {
		t.Errorf("reverted = %v\nwant %v (every hydrated file and every dir, never the stub)", reverted, want)
	}
	if len(rec.deletes) != 0 {
		t.Errorf("server touched: DELETE %v", rec.deletes)
	}
	told := false
	for _, r := range rec.reports {
		if r.kind == "unshared" && r.path == "Team" && r.err == nil {
			told = true
		}
		if r.kind == "delete-local" {
			t.Errorf("reported as a plain local delete: %+v", r)
		}
	}
	if !told {
		t.Errorf("no 'unshared' report: %+v", rec.reports)
	}
	if _, err := os.Stat(filepath.Join(w.root, "mine.txt")); err != nil {
		t.Error("an unrelated file was touched")
	}
	// Everything recorded under the share is forgotten once the copy is out:
	// a stale etag would let the mount-state heal read a later folder of the
	// same name as server content.
	if got := rec.forgottenPaths(); len(got) != 1 || got[0] != "Team" {
		t.Errorf("Ops.Forget calls = %v, want [Team]", got)
	}
}

// A share the user never opened is all stubs: there are no bytes to keep, so
// the tree goes — reported honestly, and still without a server delete.
func TestReconcileRemovesAnUnsharedFolderOfStubs(t *testing.T) {
	f, rec, w, team := unshareWorld(t)
	for _, rel := range []string{"Budget.xlsx", "old/n.txt"} {
		f.markDehydrated(filepath.Join(team, filepath.FromSlash(rel)))
	}

	w.Reconcile()

	if _, err := os.Stat(team); err == nil {
		t.Error("stub-only share left behind")
	}
	if len(rec.detachedCalls()) != 0 {
		t.Error("nothing to keep, yet Ops.Detached was called")
	}
	told := false
	for _, r := range rec.reports {
		if r.kind == "unshared-empty" && r.path == "Team" && r.err == nil {
			told = true
		}
	}
	if !told {
		t.Errorf("no 'unshared-empty' report: %+v", rec.reports)
	}
	if len(rec.deletes) != 0 {
		t.Errorf("server touched: DELETE %v", rec.deletes)
	}
	// Nothing kept, but the stores still carried the share's etags and its
	// root mark — seen live on the VM: a folder later created under the same
	// name would have been healed into a placeholder and then parked as
	// "unshared". Forget it exactly as after a park.
	if got := rec.forgottenPaths(); len(got) != 1 || got[0] != "Team" {
		t.Errorf("Ops.Forget calls = %v, want [Team]", got)
	}
}

// If parking fails the folder stays where it is, the pass does not record the
// parent's etag (so it comes back here next time), and nothing is removed.
func TestReconcileUnshareKeepsTheFolderWhenParkingFails(t *testing.T) {
	_, rec, w, team := unshareWorld(t)
	rec.detachedErr = errors.New("disk full")
	// Put the share under a listed subfolder so there is a parent etag to watch.
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("Projects", true, "e-proj", "")}
	rec.listing["Projects"] = nil
	rec.mountRoots = map[string]bool{"Projects/Team": true}
	proj := filepath.Join(w.root, "Projects")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(team, filepath.Join(proj, "Team")); err != nil {
		t.Fatal(err)
	}

	w.Reconcile()

	if _, err := os.Stat(filepath.Join(proj, "Team", "Budget.xlsx")); err != nil {
		t.Error("folder removed although parking failed")
	}
	if _, ok := rec.baselines["Projects"]; ok {
		t.Error("parent etag recorded despite an unfinished subtree — the next pass would skip it")
	}
	if len(rec.deletes) != 0 {
		t.Errorf("server touched: DELETE %v", rec.deletes)
	}
	// The copy is still inside the mount, so it must keep its root mark for
	// the retry: nothing may be forgotten yet.
	if got := rec.forgottenPaths(); len(got) != 0 {
		t.Errorf("Ops.Forget called although parking failed: %v", got)
	}
}

// A single FILE shared with the user is a mount root of its own. Hydrated —
// or edited locally and never uploaded — it is salvaged and parked, not
// deleted and not uploaded to a path that no longer exists.
func TestReconcileParksAnUnsharedFile(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	doc := filepath.Join(root, "Budget.xlsx")
	if err := os.WriteFile(doc, []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDirty(doc) // local edit pending — the old code would try to upload it forever
	rec := newRecorder()
	rec.listing[""] = nil
	rec.mountRoots = map[string]bool{"Budget.xlsx": true}
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	if got := rec.detachedCalls(); len(got) != 1 || got[0] != [2]string{doc, "Budget.xlsx"} {
		t.Fatalf("Ops.Detached calls = %v", got)
	}
	if _, err := os.Stat(doc); err != nil {
		t.Error("shared file lost")
	}
	if len(f.revertedPaths()) != 1 {
		t.Errorf("reverted = %v, want just the file", f.revertedPaths())
	}
	w.mu.Lock()
	queued := len(w.upload)
	w.mu.Unlock()
	if queued != 0 {
		t.Error("an upload was queued for a file whose share is gone")
	}
}

// Only a recorded mount root is treated this way: an ordinary in-sync folder
// the owner deleted still mirrors as before.
func TestReconcileUnshareIgnoresNonRoots(t *testing.T) {
	_, rec, w, team := unshareWorld(t)
	rec.mountRoots = map[string]bool{}

	w.Reconcile()

	if _, err := os.Stat(team); err == nil {
		t.Error("a deleted non-share folder was kept")
	}
	if len(rec.detachedCalls()) != 0 {
		t.Error("Ops.Detached called for a non-root")
	}
}
