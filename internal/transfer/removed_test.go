package transfer

// A folder the server deleted is removed here to the Recycle Bin, as the undo
// for a deletion this computer never chose. But the bin keeps nothing bigger
// than its own capacity: Windows deletes such an item outright, and with the
// confirmation suppressed it does so silently. That is how 219 GB of "To Sort"
// vanished for good (Deck #691). What the bin can't keep is moved aside, next
// to the sync folder, and the user is told where.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/otherworld/nimbo/internal/engine"
	"github.com/otherworld/nimbo/internal/state"
)

// removedFixture builds <tmp>/Sync/Big/a.txt with a baseline, and stubs the
// Recycle Bin: its capacity is capacity bytes, and recycling records the path
// (and removes it, as the real bin would) unless recycleErr is set.
func removedFixture(t *testing.T, capacity int64, recycleErr error) (ex *Executor, root string, recycled *[]string, moved *[]string) {
	t.Helper()
	root = filepath.Join(t.TempDir(), "Sync")
	if err := os.MkdirAll(filepath.Join(root, "Big"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Big", "a.txt"), make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"), "acct", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, p := range []string{"Big", "Big/a.txt"} {
		if err := st.UpsertBaseline("P", engine.BaselineState{Path: p}); err != nil {
			t.Fatal(err)
		}
	}
	var rec, mv []string
	oc, or := binCapacity, recycleFn
	binCapacity = func(string) (int64, bool) { return capacity, true }
	recycleFn = func(p string) error {
		if recycleErr != nil {
			return recycleErr
		}
		rec = append(rec, p)
		return os.RemoveAll(p)
	}
	t.Cleanup(func() { binCapacity, recycleFn = oc, or })
	ex = &Executor{State: st, PairKey: "P", LocalRoot: root,
		OnMovedAside: func(rel, dest string) { mv = append(mv, rel+" -> "+dest) }}
	return ex, root, &rec, &mv
}

func deleteBig(t *testing.T, ex *Executor) error {
	t.Helper()
	return ex.applyDelete(context.Background(), engine.Action{Kind: engine.ActDeleteLocal, Path: "Big"})
}

func TestMirroredDeleteMovesAsideWhatTheBinCannotTake(t *testing.T) {
	ex, root, recycled, moved := removedFixture(t, 10, nil) // a 10-byte bin
	if err := deleteBig(t, ex); err != nil {
		t.Fatalf("applyDelete: %v", err)
	}
	if len(*recycled) != 0 {
		t.Fatalf("recycled what the bin can't keep: %v", *recycled)
	}
	if _, err := os.Stat(filepath.Join(root, "Big")); !os.IsNotExist(err) {
		t.Fatalf("still in the sync folder (err=%v)", err)
	}
	aside := filepath.Join(filepath.Dir(root), "Sync - removed on server", "Big", "a.txt")
	if _, err := os.Stat(aside); err != nil {
		t.Fatalf("not moved aside to %s: %v", aside, err)
	}
	if len(*moved) != 1 || !strings.Contains((*moved)[0], "Sync - removed on server") {
		t.Fatalf("the user was not told where it went: %v", *moved)
	}
}

func TestMirroredDeleteRecyclesWhatTheBinCanTake(t *testing.T) {
	ex, root, recycled, moved := removedFixture(t, 1<<30, nil)
	if err := deleteBig(t, ex); err != nil {
		t.Fatalf("applyDelete: %v", err)
	}
	if len(*recycled) != 1 || len(*moved) != 0 {
		t.Fatalf("recycled=%v moved=%v, want recycled only", *recycled, *moved)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "Sync - removed on server")); !os.IsNotExist(err) {
		t.Fatal("a removed-on-server folder was made for something the bin took")
	}
}

func TestMirroredDeleteMovesAsideWhenTheBinRefuses(t *testing.T) {
	ex, root, _, moved := removedFixture(t, 1<<30, errors.New("the bin said no"))
	if err := deleteBig(t, ex); err != nil {
		t.Fatalf("applyDelete: %v", err)
	}
	if len(*moved) != 1 {
		t.Fatalf("a refused recycle must move the folder aside, moved=%v", *moved)
	}
	if _, err := os.Stat(filepath.Join(root, "Big")); !os.IsNotExist(err) {
		t.Fatalf("still in the sync folder (err=%v)", err)
	}
}

// If it can't even be moved aside, it stays where it is: the action fails and
// is tried again, rather than falling back to a delete nobody can undo.
func TestMirroredDeleteKeepsTheFolderWhenItCannotMoveIt(t *testing.T) {
	ex, root, _, _ := removedFixture(t, 10, nil)
	blocker := filepath.Join(filepath.Dir(root), "Sync - removed on server")
	if err := os.WriteFile(blocker, []byte("a file where the folder would go"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := deleteBig(t, ex); err == nil {
		t.Fatal("a folder that could not be moved aside was reported removed")
	}
	if _, err := os.Stat(filepath.Join(root, "Big", "a.txt")); err != nil {
		t.Fatalf("the folder was lost: %v", err)
	}
	if rows, _ := ex.State.LoadBaseline("P"); rows["Big/a.txt"].Path == "" {
		t.Fatal("its baseline went although the folder stayed")
	}
}

// A folder deleted on the server usually arrives as one delete per file (a
// full or delta pass lists everything under it). Each file fits the bin on
// its own, but the bin makes room by purging its oldest items, so recycling
// them one by one lost most of a folder bigger than the bin just the same.
// The bin takes files only until this pass has filled nine tenths of it.
func TestAFolderDeletedFileByFileStopsRecyclingWhenTheBinWouldFill(t *testing.T) {
	ex, root, recycled, moved := removedFixture(t, 300, nil) // 300-byte bin: 270 usable
	for _, n := range []string{"b.txt", "c.txt", "d.txt", "e.txt"} {
		if err := os.WriteFile(filepath.Join(root, "Big", n), make([]byte, 100), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var actions []engine.Action
	for _, n := range []string{"a.txt", "b.txt", "c.txt", "d.txt", "e.txt"} {
		actions = append(actions, engine.Action{Kind: engine.ActDeleteLocal, Path: "Big/" + n})
	}
	actions = append(actions, engine.Action{Kind: engine.ActDeleteLocal, Path: "Big"})
	if _, err := ex.Run(context.Background(), actions); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var files int
	for _, p := range *recycled {
		if strings.HasSuffix(p, ".txt") {
			files++
		}
	}
	if files != 2 || len(*moved) != 3 {
		t.Fatalf("recycled %d files and moved %d aside, want 2 and 3: recycled=%v moved=%v", files, len(*moved), *recycled, *moved)
	}
}

// A drive with no Recycle Bin (a network or removable drive), or one whose bin
// the user switched off, always deleted permanently, and still does: moving
// every deletion aside there would pile up without end.
func TestADriveWithNoBinDeletesAsBefore(t *testing.T) {
	ex, root, recycled, moved := removedFixture(t, 0, nil) // capacity 0: no bin here
	if err := deleteBig(t, ex); err != nil {
		t.Fatalf("applyDelete: %v", err)
	}
	if len(*recycled) != 0 || len(*moved) != 0 {
		t.Fatalf("recycled=%v moved=%v, want a plain delete", *recycled, *moved)
	}
	if _, err := os.Stat(filepath.Join(root, "Big")); !os.IsNotExist(err) {
		t.Fatalf("not deleted (err=%v)", err)
	}
}

// When the bin's capacity can't be read on a drive that has one, that is not
// "no bin": it read as one, so the item was deleted for good. Unknown means
// the safe choice, moving it aside.
func TestAnUnknownBinCapacityMovesAside(t *testing.T) {
	ex, root, recycled, moved := removedFixture(t, binUnknown, nil)
	if err := deleteBig(t, ex); err != nil {
		t.Fatalf("applyDelete: %v", err)
	}
	if len(*recycled) != 0 || len(*moved) != 1 {
		t.Fatalf("recycled=%v moved=%v, want it moved aside", *recycled, *moved)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "Sync - removed on server", "Big", "a.txt")); err != nil {
		t.Fatalf("not moved aside: %v", err)
	}
}
