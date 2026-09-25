package agent

import (
	"testing"

	"github.com/otherworld/nimbo/internal/account"
	"github.com/otherworld/nimbo/internal/config"
	"github.com/otherworld/nimbo/internal/transport"
)

func goneEngine() *Engine {
	return &Engine{
		Account: account.Account{LoginName: "test"},
		caps:    lockingCaps(),
		locked:  map[string][]LockedFile{},
		lockMgr: newLockMgr(nil, config.Dirs{}, "test"),
	}
}

// A folder deleted on the server takes its locked files with it. The parent's
// listing no longer has the folder, and that is the only listing that will ever
// say so, because the folder itself is never listed again (#744).
func TestListingDropsALockUnderADeletedFolder(t *testing.T) {
	e := goneEngine()
	e.locked["M"] = []LockedFile{lf("I7-Locks/fresh.txt", "adam"), lf("Keep/a.txt", "adam")}

	// The root, listed after I7-Locks was deleted: only folders, and Keep is still there.
	e.NoteRemoteLocks("M", "", "", []transport.Entry{
		{Path: "", IsDir: true},
		{Path: "Keep", IsDir: true},
		{Path: "Other", IsDir: true},
	})
	got := e.LockedFiles()
	if len(got) != 1 || got[0].Path != "Keep/a.txt" {
		t.Fatalf("locked = %+v, want only Keep/a.txt", got)
	}
}

// Deeper down, and under a mount with a remote root: the listing of A lacks B,
// so a lock on A/B/c/d.txt is gone, while one on A/C/e.txt is not ours to judge.
func TestListingDropsNestedLocksUnderARemoteRoot(t *testing.T) {
	e := goneEngine()
	e.locked["M"] = []LockedFile{lf("A/B/c/d.txt", "adam"), lf("A/C/e.txt", "adam"), lf("Z/f.txt", "adam")}

	e.NoteRemoteLocks("M", "Team", "Team/A", []transport.Entry{
		{Path: "Team/A", IsDir: true},
		{Path: "Team/A/C", IsDir: true},
	})
	got := map[string]bool{}
	for _, f := range e.LockedFiles() {
		got[f.Path] = true
	}
	if got["A/B/c/d.txt"] || !got["A/C/e.txt"] || !got["Z/f.txt"] {
		t.Fatalf("locked = %v, want A/C/e.txt and Z/f.txt only", got)
	}
}

// A live pass that deletes a folder locally drops the locks inside it; a file
// that only shares the folder's name as a prefix stays.
func TestLiveDeleteDropsLocksUnderIt(t *testing.T) {
	e := goneEngine()
	e.locked["P"] = []LockedFile{lf("Old/a.xlsx", "adam"), lf("Old/sub/b.xlsx", "adam"), lf("Older/c.xlsx", "adam"), lf("gone.docx", "adam")}

	gone := e.locksUnder("P", []string{"Old", "gone.docx"})
	e.reconcileLockedGone("P", nil, nil, gone)
	got := e.LockedFiles()
	if len(got) != 1 || got[0].Path != "Older/c.xlsx" {
		t.Fatalf("locked = %+v, want only Older/c.xlsx", got)
	}
}

func lockingCaps() *transport.Capabilities {
	c := &transport.Capabilities{}
	c.Files.Locking = "1.0"
	return c
}
