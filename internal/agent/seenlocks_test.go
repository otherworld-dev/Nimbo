package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/otherworld/nimbo/internal/config"
)

func seenLockEngine(t *testing.T) *Engine {
	t.Helper()
	d := config.Dirs{Config: t.TempDir(), Data: t.TempDir()}.WithAccount("acc1")
	return &Engine{dirs: d, locked: make(map[string][]LockedFile), lockToast: make(map[string]time.Time)}
}

// Every change to the locked set is written through, so the next start can put
// it back (GitHub #7): a colleague's lock took a long time to reappear after a
// restart because an unchanged folder is never listed again.
func TestLockedSetIsPersisted(t *testing.T) {
	e := seenLockEngine(t)
	e.reconcileLocked(`C:\Sync`, paths("Team/Budget.xlsx"), []LockedFile{{
		Path: "Team/Budget.xlsx", Owner: "bob", OwnerDisplay: "Bob", Since: time.Unix(1790368000, 0),
	}})
	got := e.dirs.LoadSeenLocks()
	if len(got) != 1 || got[0].LocalDir != `C:\Sync` || got[0].Path != "Team/Budget.xlsx" ||
		got[0].OwnerDisplay != "Bob" || !got[0].Since.Equal(time.Unix(1790368000, 0)) {
		t.Fatalf("saved = %+v", got)
	}

	e.reconcileLocked(`C:\Sync`, paths("Team/Budget.xlsx"), nil)
	if got := e.dirs.LoadSeenLocks(); len(got) != 0 {
		t.Fatalf("a released lock is still saved: %+v", got)
	}
}

// Only locks on files that still exist, in a folder the account still syncs,
// come back. Anything else would sit in the In use list for good, because
// nothing would ever list it again to clear it.
func TestRestoreSeenLocksKeepsOnlyCurrentFiles(t *testing.T) {
	e := seenLockEngine(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Team"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Team", "Budget.xlsx"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.dirs.SavePairs([]config.SyncPair{{LocalDir: root, RemoteRoot: "/"}}); err != nil {
		t.Fatal(err)
	}
	gone := t.TempDir() // a folder the account no longer syncs
	if err := os.WriteFile(filepath.Join(gone, "old.xlsx"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.dirs.SaveSeenLocks([]config.SeenLock{
		{LocalDir: root, Path: "Team/Budget.xlsx", Owner: "bob"},
		{LocalDir: root, Path: "Team/Deleted.xlsx", Owner: "bob"},
		{LocalDir: gone, Path: "old.xlsx", Owner: "bob"},
	}); err != nil {
		t.Fatal(err)
	}

	e.restoreSeenLocks()
	got := e.LockedFiles()
	if len(got) != 1 || got[0].Path != "Team/Budget.xlsx" || got[0].Owner != "bob" {
		t.Fatalf("restored = %+v, want only Team/Budget.xlsx", got)
	}
	// What was dropped is dropped from the file too.
	if saved := e.dirs.LoadSeenLocks(); len(saved) != 1 {
		t.Errorf("saved after restore = %+v, want the one kept", saved)
	}
}

// The on-demand folder has no sync pairs, so its root comes from the account's
// folder setup.
func TestRestoreSeenLocksAcceptsTheOnDemandRoot(t *testing.T) {
	e := seenLockEngine(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.docx"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.dirs.UpdateAccountState(func(s *config.AccountState) { s.OnDemandRoot = root }); err != nil {
		t.Fatal(err)
	}
	if err := e.dirs.SaveSeenLocks([]config.SeenLock{{LocalDir: root, Path: "a.docx", Owner: "bob"}}); err != nil {
		t.Fatal(err)
	}
	e.restoreSeenLocks()
	if got := e.LockedFiles(); len(got) != 1 {
		t.Fatalf("restored = %+v, want a.docx", got)
	}
}

// A restored lock was announced before the restart. Seeing it again in the
// first listing must not toast a second time.
func TestRestoredLockDoesNotToastAgain(t *testing.T) {
	e := seenLockEngine(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.docx"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.dirs.SavePairs([]config.SyncPair{{LocalDir: root, RemoteRoot: "/"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.dirs.SaveSeenLocks([]config.SeenLock{{LocalDir: root, Path: "a.docx", Owner: "bob"}}); err != nil {
		t.Fatal(err)
	}
	toasts := 0
	e.onToast = func(string, string, string) { toasts++ }
	e.restoreSeenLocks()
	e.reconcileLocked(root, paths("a.docx"), []LockedFile{lf("a.docx", "bob")})
	if toasts != 0 {
		t.Errorf("toasted %d times for a lock already announced before the restart", toasts)
	}
	// A lock by someone new on the same file is news.
	e.reconcileLocked(root, paths("a.docx"), []LockedFile{lf("a.docx", "carol")})
	if toasts != 1 {
		t.Errorf("toasts = %d after carol took the lock, want 1", toasts)
	}
}
