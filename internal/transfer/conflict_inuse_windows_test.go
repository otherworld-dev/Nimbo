package transfer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/otherworld/nimbo/internal/engine"
)

// A .pst Outlook has open, and any file a program has locked part of, cannot be
// read to compare it with the server's copy. That was turned into a conflict
// the user could not clear (Deck #714). It waits in "In use" like an upload of
// it would, and is looked at again once the program lets go.
func TestConflictCheckWaitsForAPstOutlookHasOpen(t *testing.T) {
	c, gets := conflictServer(t, 0)
	ex := conflictExecutor(t, c)
	pst := filepath.Join(ex.LocalRoot, "archive.pst")
	if err := os.WriteFile(pst, []byte("mail"), 0o644); err != nil {
		t.Fatal(err)
	}
	ex.Remote["archive.pst"] = engine.RemoteState{Path: "archive.pst", ETag: "e2"}
	outlook, err := os.OpenFile(pst, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer outlook.Close()

	_, merged, err := ex.classifyConflict(context.Background(), engine.Action{Kind: engine.ActConflict, Path: "archive.pst"})
	var inUse *InUseError
	if !errors.As(err, &inUse) || merged {
		t.Fatalf("err = %v merged = %v, want an InUseError", err, merged)
	}
	if gets.Load() != 0 {
		t.Errorf("downloaded the server copy of a file it could not compare")
	}
}

func TestConflictCheckWaitsForAFileAProgramHasLocked(t *testing.T) {
	c, gets := conflictServer(t, 0)
	ex := conflictExecutor(t, c)
	f, err := os.OpenFile(filepath.Join(ex.LocalRoot, "doc.txt"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var ol windows.Overlapped
	if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &ol); err != nil {
		t.Fatal(err)
	}
	defer windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &ol)

	_, _, err = ex.classifyConflict(context.Background(), engine.Action{Kind: engine.ActConflict, Path: "doc.txt"})
	var inUse *InUseError
	if !errors.As(err, &inUse) {
		t.Fatalf("err = %v, want an InUseError", err)
	}
	if gets.Load() != 0 {
		t.Errorf("downloaded the server copy of a file it could not compare")
	}
}
