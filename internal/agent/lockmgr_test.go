package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/otherworld/nimbo/internal/config"
	"github.com/otherworld/nimbo/internal/transport"
)

type fakeLocker struct {
	locked    map[string]bool
	lockErr   map[string]error
	unlockErr map[string]error
	unlocked  []string
	lockCalls []string
}

func newFakeLocker() *fakeLocker {
	return &fakeLocker{
		locked: map[string]bool{}, lockErr: map[string]error{}, unlockErr: map[string]error{},
	}
}

func (f *fakeLocker) Lock(_ context.Context, p string) (transport.LockResult, error) {
	f.lockCalls = append(f.lockCalls, p)
	if err := f.lockErr[p]; err != nil {
		return transport.LockResult{}, err
	}
	f.locked[p] = true
	return transport.LockResult{Token: "files_lock/" + p, ETag: "e-" + p}, nil
}

func (f *fakeLocker) Unlock(_ context.Context, p string) error {
	if err := f.unlockErr[p]; err != nil {
		return err
	}
	f.unlocked = append(f.unlocked, p)
	delete(f.locked, p)
	return nil
}

func newTestMgr(t *testing.T) (*lockMgr, *fakeLocker, config.Dirs) {
	t.Helper()
	d := config.Dirs{Config: t.TempDir(), Data: t.TempDir()}
	f := newFakeLocker()
	return newLockMgr(f, d, "adam"), f, d
}

// Every lock is on disk before it is taken, so a crash cannot strand one
// invisibly — the server never expires locks, so the registry is the only
// record that will survive.
func TestTakeLockPersists(t *testing.T) {
	m, f, d := newTestMgr(t)
	if err := m.take(context.Background(), "Team/Budget.xlsx"); err != nil {
		t.Fatal(err)
	}
	if !f.locked["Team/Budget.xlsx"] {
		t.Error("server was never asked to lock it")
	}
	held, _ := d.LoadHeldLocks()
	if len(held) != 1 || held[0].RemotePath != "Team/Budget.xlsx" {
		t.Fatalf("registry = %+v, want the lock recorded on disk", held)
	}
	if held[0].Token != "files_lock/Team/Budget.xlsx" {
		t.Errorf("token not recorded: %+v", held[0])
	}
	if held[0].Account != "adam" {
		t.Errorf("account = %q, want adam", held[0].Account)
	}
}

// A LOCK that fails must leave nothing behind, or every future sweep retries a
// lock that was never taken.
func TestTakeLockFailureLeavesNoRecord(t *testing.T) {
	m, f, d := newTestMgr(t)
	f.lockErr["nope.xlsx"] = errors.New(`LOCK "nope.xlsx": server returned 423 Locked: `)

	if err := m.take(context.Background(), "nope.xlsx"); err == nil {
		t.Fatal("take() returned nil for a failing LOCK")
	}
	if held, _ := d.LoadHeldLocks(); len(held) != 0 {
		t.Fatalf("registry = %+v, want empty after a failed lock", held)
	}
	if len(m.list()) != 0 {
		t.Errorf("in-memory set = %+v, want empty", m.list())
	}
}

// Release unlocks then forgets. If the UNLOCK fails the record must SURVIVE, so
// a later sweep can try again — forgetting first would strand a real lock.
func TestReleaseKeepsRecordWhenUnlockFails(t *testing.T) {
	m, f, d := newTestMgr(t)
	ctx := context.Background()
	if err := m.take(ctx, "a.xlsx"); err != nil {
		t.Fatal(err)
	}
	f.unlockErr["a.xlsx"] = errors.New("network went away")

	if err := m.release(ctx, "a.xlsx"); err == nil {
		t.Fatal("release() returned nil despite a failing UNLOCK")
	}
	if held, _ := d.LoadHeldLocks(); len(held) != 1 {
		t.Fatalf("registry = %+v, want the lock still recorded so a sweep retries it", held)
	}

	// Once the server cooperates, it goes.
	delete(f.unlockErr, "a.xlsx")
	if err := m.release(ctx, "a.xlsx"); err != nil {
		t.Fatal(err)
	}
	if held, _ := d.LoadHeldLocks(); len(held) != 0 {
		t.Fatalf("registry = %+v, want empty", held)
	}
}

// We must never UNLOCK something we did not take: it is not ours to clear and
// the server answers 423.
func TestReleaseIgnoresLocksWeDoNotHold(t *testing.T) {
	m, f, _ := newTestMgr(t)
	if err := m.release(context.Background(), "someone-elses.xlsx"); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(f.unlocked) != 0 {
		t.Errorf("sent UNLOCK for a lock we never took: %v", f.unlocked)
	}
}

// The sweep is what saves a colleague from a permanently locked file after we
// crash. It releases this account's strays and forgets them.
func TestSweepReleasesStrandedLocks(t *testing.T) {
	m, f, d := newTestMgr(t)
	_ = d.SaveHeldLocks([]config.HeldLock{
		{Account: "adam", RemotePath: "stranded.xlsx", Token: "files_lock/x"},
	})

	n, err := m.sweep(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("sweep = %d, %v; want 1, nil", n, err)
	}
	if len(f.unlocked) != 1 || f.unlocked[0] != "stranded.xlsx" {
		t.Fatalf("unlocked = %v", f.unlocked)
	}
	if held, _ := d.LoadHeldLocks(); len(held) != 0 {
		t.Fatalf("registry still holds %+v after a sweep", held)
	}
}

// Another account's strays are not ours to release, and must survive the sweep.
func TestSweepIgnoresOtherAccounts(t *testing.T) {
	m, f, d := newTestMgr(t)
	_ = d.SaveHeldLocks([]config.HeldLock{
		{Account: "someone-else", RemotePath: "theirs.xlsx", Token: "files_lock/y"},
	})

	n, err := m.sweep(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("sweep = %d, %v; want 0, nil", n, err)
	}
	if len(f.unlocked) != 0 {
		t.Errorf("released another account's lock: %v", f.unlocked)
	}
	if held, _ := d.LoadHeldLocks(); len(held) != 1 {
		t.Fatalf("another account's entry was dropped: %+v", held)
	}
}

// A 423 during the sweep means the lock became somebody else's. Retrying it
// every boot forever would be pointless noise, so it is forgotten.
func TestSweepForgetsLocksNowHeldByOthers(t *testing.T) {
	m, f, d := newTestMgr(t)
	_ = d.SaveHeldLocks([]config.HeldLock{{Account: "adam", RemotePath: "taken.xlsx"}})
	f.unlockErr["taken.xlsx"] = errors.New(`UNLOCK "taken.xlsx": server returned 423 Locked: `)

	if _, err := m.sweep(context.Background()); err == nil {
		t.Fatal("sweep() hid a 423")
	}
	if held, _ := d.LoadHeldLocks(); len(held) != 0 {
		t.Fatalf("registry = %+v, want the 423 entry forgotten", held)
	}
}

// A transient failure during the sweep must be retried next boot, so the entry
// has to stay.
func TestSweepKeepsTransientFailures(t *testing.T) {
	m, f, d := newTestMgr(t)
	_ = d.SaveHeldLocks([]config.HeldLock{{Account: "adam", RemotePath: "flaky.xlsx"}})
	f.unlockErr["flaky.xlsx"] = errors.New("connection refused")

	if _, err := m.sweep(context.Background()); err == nil {
		t.Fatal("sweep() hid a failure")
	}
	if held, _ := d.LoadHeldLocks(); len(held) != 1 {
		t.Fatalf("registry = %+v, want the entry kept for a retry", held)
	}
}

// Multi-account: our own locks and another account's coexist in one file.
func TestPersistPreservesOtherAccounts(t *testing.T) {
	m, _, d := newTestMgr(t)
	_ = d.SaveHeldLocks([]config.HeldLock{{Account: "other", RemotePath: "theirs.xlsx"}})

	if err := m.take(context.Background(), "mine.xlsx"); err != nil {
		t.Fatal(err)
	}
	held, _ := d.LoadHeldLocks()
	if len(held) != 2 {
		t.Fatalf("registry = %+v, want both accounts", held)
	}
	byAcct := map[string]string{}
	for _, l := range held {
		byAcct[l.Account] = l.RemotePath
	}
	if byAcct["other"] != "theirs.xlsx" || byAcct["adam"] != "mine.xlsx" {
		t.Errorf("entries = %+v", held)
	}
}

// Taking a lock we already hold is a refresh, not a duplicate.
func TestTakeTwiceRefreshes(t *testing.T) {
	m, f, d := newTestMgr(t)
	ctx := context.Background()
	_ = m.take(ctx, "a.xlsx")
	_ = m.take(ctx, "a.xlsx")

	if len(f.lockCalls) != 2 {
		t.Errorf("lock calls = %v, want two (the second being the refresh)", f.lockCalls)
	}
	if held, _ := d.LoadHeldLocks(); len(held) != 1 {
		t.Fatalf("registry = %+v, want a single entry", held)
	}
}

// releaseAll is both the shutdown path and the user's escape hatch.
func TestReleaseAll(t *testing.T) {
	m, f, d := newTestMgr(t)
	ctx := context.Background()
	_ = m.take(ctx, "a.xlsx")
	_ = m.take(ctx, "b.xlsx")

	n, err := m.releaseAll(ctx)
	if err != nil || n != 2 {
		t.Fatalf("releaseAll = %d, %v; want 2, nil", n, err)
	}
	if len(f.unlocked) != 2 {
		t.Errorf("unlocked = %v", f.unlocked)
	}
	if held, _ := d.LoadHeldLocks(); len(held) != 0 {
		t.Fatalf("registry = %+v, want empty", held)
	}
}

// The heartbeat re-LOCKs what we hold; a 423 means it is no longer ours.
func TestHeartbeatDropsStolenLocks(t *testing.T) {
	m, f, d := newTestMgr(t)
	ctx := context.Background()
	_ = m.take(ctx, "a.xlsx")
	_ = m.take(ctx, "b.xlsx")

	f.lockErr["a.xlsx"] = errors.New(`LOCK "a.xlsx": server returned 423 Locked: `)
	m.heartbeat(ctx)

	held, _ := d.LoadHeldLocks()
	if len(held) != 1 || held[0].RemotePath != "b.xlsx" {
		t.Fatalf("registry = %+v, want only b.xlsx", held)
	}
}

func TestListReportsHeld(t *testing.T) {
	m, _, _ := newTestMgr(t)
	m.now = func() time.Time { return time.Unix(1786235387, 0) }
	_ = m.take(context.Background(), "a.xlsx")

	got := m.list()
	if len(got) != 1 || got[0].RemotePath != "a.xlsx" {
		t.Fatalf("list = %+v", got)
	}
	if !got[0].Taken.Equal(time.Unix(1786235387, 0)) {
		t.Errorf("Taken = %v", got[0].Taken)
	}
}

// documentForLockFile is where the Word naming trap bites: the owner name is
// many-to-one, so an ambiguous one must resolve to nothing rather than to a
// document the user never opened.
func TestDocumentForLockFile(t *testing.T) {
	dir := []string{"Annual Report.docx", "Budget.xlsx", "notes.odt", "0cdefgh.docx", "12cdefgh.docx"}

	if got, ok := documentForLockFile("~$nual Report.docx", dir); !ok || got != "Annual Report.docx" {
		t.Errorf("Word owner file: got %q, %v", got, ok)
	}
	if got, ok := documentForLockFile("~$Budget.xlsx", dir); !ok || got != "Budget.xlsx" {
		t.Errorf("Excel owner file: got %q, %v", got, ok)
	}
	if got, ok := documentForLockFile(".~lock.notes.odt#", dir); !ok || got != "notes.odt" {
		t.Errorf("LibreOffice lock file: got %q, %v", got, ok)
	}
	if got, ok := documentForLockFile("~$cdefgh.docx", dir); ok {
		t.Errorf("ambiguous owner file resolved to %q; want a refusal", got)
	}
	if _, ok := documentForLockFile("Budget.xlsx", dir); ok {
		t.Error("an ordinary document must not be treated as a lock file")
	}
	if _, ok := documentForLockFile("~$ghost.docx", dir); ok {
		t.Error("an owner file with no matching document must not resolve")
	}
}

// The flyout must not claim "Up to date" when the only reason a pass did
// nothing was that somebody else has the file open — the user's change is
// deliberately unsynced and they need to know before closing the laptop.
func TestHeldStatus(t *testing.T) {
	if got := heldStatus(nil); got != "Up to date" {
		t.Errorf("no holds: %q", got)
	}
	got := heldStatus([]string{"Team/Budget.xlsx"})
	if got != "Waiting — Budget.xlsx is in use by someone else" {
		t.Errorf("one hold: %q", got)
	}
	if got := heldStatus([]string{"a.xlsx", "b.docx"}); got != "Waiting — 2 files are in use by someone else" {
		t.Errorf("two holds: %q", got)
	}
}

// On-demand mode has no sync pairs, so it feeds lock state in through its
// directory listings instead. Paths must come out relative to the mount so the
// absolute path resolves, and a non-empty remote root must be stripped.
func TestNoteRemoteLocks(t *testing.T) {
	d := config.Dirs{Config: t.TempDir()}
	e := &Engine{
		locked: make(map[string][]LockedFile), lockToast: make(map[string]time.Time),
		dirs: d, caps: &transport.Capabilities{},
	}
	e.caps.Files.Locking = "1.0"
	e.Account.LoginName = "alice"
	e.lockMgr = newLockMgr(newFakeLocker(), d, "alice")

	entries := []transport.Entry{
		{Path: "Team/Budget.xlsx", Lock: &transport.LockInfo{Owner: "bob", OwnerDisplay: "Bob"}},
		{Path: "Team/Mine.xlsx", Lock: &transport.LockInfo{Owner: "alice"}},
		{Path: "Team/Free.xlsx"},
		{Path: "Team", IsDir: true, Lock: &transport.LockInfo{Owner: "bob"}},
	}
	e.NoteRemoteLocks(`C:\Sync`, "", entries)

	got := e.LockedFiles()
	if len(got) != 1 {
		t.Fatalf("LockedFiles = %+v, want just Budget.xlsx", got)
	}
	if got[0].Path != "Team/Budget.xlsx" {
		t.Errorf("Path = %q", got[0].Path)
	}
	if got[0].Abs != `C:\Sync\Team\Budget.xlsx` {
		t.Errorf("Abs = %q, want it resolved against the mount", got[0].Abs)
	}

	// A later listing of the same folder that finds it free clears it.
	e.NoteRemoteLocks(`C:\Sync`, "", []transport.Entry{{Path: "Team/Budget.xlsx"}})
	if got := e.LockedFiles(); len(got) != 0 {
		t.Errorf("LockedFiles = %+v after release, want none", got)
	}
}

// A remote root must be stripped, or every path would resolve outside the mount.
func TestNoteRemoteLocksStripsRoot(t *testing.T) {
	d := config.Dirs{Config: t.TempDir()}
	e := &Engine{
		locked: make(map[string][]LockedFile), lockToast: make(map[string]time.Time),
		dirs: d, caps: &transport.Capabilities{},
	}
	e.caps.Files.Locking = "1.0"
	e.Account.LoginName = "alice"
	e.lockMgr = newLockMgr(newFakeLocker(), d, "alice")

	e.NoteRemoteLocks(`C:\Sync`, "Work", []transport.Entry{
		{Path: "Work/Budget.xlsx", Lock: &transport.LockInfo{Owner: "bob"}},
		{Path: "Elsewhere/Other.xlsx", Lock: &transport.LockInfo{Owner: "bob"}},
	})
	got := e.LockedFiles()
	if len(got) != 1 || got[0].Path != "Budget.xlsx" {
		t.Fatalf("LockedFiles = %+v, want Budget.xlsx relative to the root", got)
	}
	if got[0].Abs != `C:\Sync\Budget.xlsx` {
		t.Errorf("Abs = %q", got[0].Abs)
	}
}
