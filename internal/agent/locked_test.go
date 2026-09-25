package agent

import (
	"testing"
	"time"

	"github.com/otherworld/nimbo/internal/engine"
	"github.com/otherworld/nimbo/internal/transport"
)

// SyncPaths is the path an Office save actually takes (watcher -> relsFor ->
// SyncPaths) and it builds RemoteState by hand from a bare Stat. Lock state must
// survive that conversion, or the feature works on a cold scan and silently does
// nothing on the path that matters.
func TestRemoteStateFromEntryCarriesLock(t *testing.T) {
	ent := transport.Entry{
		Path: "Budget.xlsx", Size: 10, ETag: "e1", FileID: "42",
		Lock: &transport.LockInfo{Owner: "bob", OwnerType: transport.LockOwnerApp},
	}
	got := remoteStateFrom("Budget.xlsx", ent, false)
	if got.Lock == nil || got.Lock.Owner != "bob" || got.Lock.OwnerType != transport.LockOwnerApp {
		t.Fatalf("Lock = %+v, want bob's app-owned lock", got.Lock)
	}
	if !got.LockKnown {
		t.Error("LockKnown = false; a Stat DID look, so its answer is authoritative")
	}
	if got.ETag != "e1" || got.FileID != "42" || got.Size != 10 || got.Path != "Budget.xlsx" {
		t.Errorf("an existing field was lost in the conversion: %+v", got)
	}

	unlocked := remoteStateFrom("Free.xlsx", transport.Entry{Path: "Free.xlsx"}, false)
	if unlocked.Lock != nil {
		t.Errorf("Lock = %+v, want nil", unlocked.Lock)
	}
}

func lf(path, owner string) LockedFile { return LockedFile{Path: path, Owner: owner} }

func paths(p ...string) map[string]bool {
	m := make(map[string]bool, len(p))
	for _, x := range p {
		m[x] = true
	}
	return m
}

// A RELEASED lock must disappear. This is the bug found on the 2026-08-09 beta:
// the locked set was merge-only, copied from the blocked-files machinery where
// merge-only is correct (a forbidden name stays forbidden). A lock is transient,
// so a pass that looked at a path and found it unlocked must REMOVE it.
func TestLockedReleasedLockIsRemoved(t *testing.T) {
	e := &Engine{locked: make(map[string][]LockedFile)}

	e.reconcileLocked("d", paths("a.xlsx"), []LockedFile{lf("a.xlsx", "bob")})
	if n := len(e.locked["d"]); n != 1 {
		t.Fatalf("after locking: %d entries, want 1", n)
	}

	// The next pass looks at the same path and finds it free.
	e.reconcileLocked("d", paths("a.xlsx"), nil)
	if n := len(e.locked["d"]); n != 0 {
		t.Fatalf("released lock still listed: %+v", e.locked["d"])
	}
	if _, ok := e.locked["d"]; ok {
		t.Error("an emptied pair should be dropped from the map entirely")
	}
}

// A pass must not touch paths it did not examine — the trap blocked_test.go
// exists for. A delta sync looks at a handful of paths; everything else is
// unknown to it, not free.
func TestLockedUnexaminedPathsSurvive(t *testing.T) {
	e := &Engine{locked: make(map[string][]LockedFile)}
	e.reconcileLocked("d", paths("a.xlsx", "b.xlsx"), []LockedFile{lf("a.xlsx", "bob"), lf("b.xlsx", "bob")})

	// A delta that only examined an unrelated path must leave both alone.
	e.reconcileLocked("d", paths("z.txt"), nil)
	if n := len(e.locked["d"]); n != 2 {
		t.Fatalf("delta wiped locks it never examined: %d, want 2", n)
	}

	// Examining exactly one of them releases exactly that one.
	e.reconcileLocked("d", paths("a.xlsx"), nil)
	got := e.locked["d"]
	if len(got) != 1 || got[0].Path != "b.xlsx" {
		t.Fatalf("locked = %+v, want only b.xlsx", got)
	}
}

// A pruned subtree is replayed from the baseline with no lock information, so it
// must not count as examined — otherwise a long-held lock vanishes from the UI
// the first time its folder's ETag is unchanged.
func TestLockedIgnoresUnknownEntries(t *testing.T) {
	e := &Engine{locked: make(map[string][]LockedFile)}
	e.reconcileLocked("d", paths("a.xlsx"), []LockedFile{lf("a.xlsx", "bob")})

	// remote carries the path, but replayed from the baseline: LockKnown false.
	examined, locked := lockScan(map[string]engine.RemoteState{
		"a.xlsx": {Path: "a.xlsx", LockKnown: false},
	}, "me", "d", "")
	if len(examined) != 0 || len(locked) != 0 {
		t.Fatalf("a replayed entry must not count as examined: examined=%v locked=%v", examined, locked)
	}
	e.reconcileLocked("d", examined, locked)
	if n := len(e.locked["d"]); n != 1 {
		t.Fatalf("an unexamined (pruned) path cleared a real lock: %d, want 1", n)
	}

	// The same path from a real listing, now unlocked, DOES clear it.
	examined, locked = lockScan(map[string]engine.RemoteState{
		"a.xlsx": {Path: "a.xlsx", LockKnown: true},
	}, "me", "d", "")
	e.reconcileLocked("d", examined, locked)
	if n := len(e.locked["d"]); n != 0 {
		t.Fatalf("a listed, unlocked path did not clear the lock: %d, want 0", n)
	}
}

// lockScan must attribute correctly: our own lock is not "someone else has it".
func TestLockScanIgnoresOwnLock(t *testing.T) {
	remote := map[string]engine.RemoteState{
		"mine.xlsx":   {Path: "mine.xlsx", LockKnown: true, Lock: &transport.LockInfo{Owner: "me"}},
		"theirs.xlsx": {Path: "theirs.xlsx", LockKnown: true, Lock: &transport.LockInfo{Owner: "bob"}},
		"adir":        {Path: "adir", IsDir: true, LockKnown: true, Lock: &transport.LockInfo{Owner: "bob"}},
	}
	examined, locked := lockScan(remote, "me", "d", "")
	if len(locked) != 1 || locked[0].Path != "theirs.xlsx" {
		t.Fatalf("locked = %+v, want only theirs.xlsx", locked)
	}
	// Both files were examined; the directory was not (locks are per-file).
	if !examined["mine.xlsx"] || !examined["theirs.xlsx"] || examined["adir"] {
		t.Errorf("examined = %v", examined)
	}
}

func TestLockedFilesAcrossPairs(t *testing.T) {
	e := &Engine{locked: make(map[string][]LockedFile)}
	e.reconcileLocked(`C:\Sync`, paths("Team/Budget.xlsx"), []LockedFile{lf("Team/Budget.xlsx", "bob")})
	e.reconcileLocked(`C:\Other`, paths("a.docx"), []LockedFile{lf("a.docx", "carol")})

	got := e.LockedFiles()
	if len(got) != 2 {
		t.Fatalf("LockedFiles() returned %d, want 2", len(got))
	}
	byOwner := map[string]LockedFile{}
	for _, f := range got {
		byOwner[f.Owner] = f
	}
	if f := byOwner["bob"]; f.Abs != `C:\Sync\Team\Budget.xlsx` {
		t.Errorf("Abs = %q, want the joined absolute path", f.Abs)
	}
	if f := byOwner["carol"]; f.LocalDir != `C:\Other` {
		t.Errorf("LocalDir = %q, want C:\\Other", f.LocalDir)
	}
}

// A lock still held must not re-toast every pass, and one that flaps (a pruned
// subtree can make it vanish and reappear) must not toast repeatedly either.
func TestLockToastOncePerLock(t *testing.T) {
	var toasts []string
	e := &Engine{
		locked:    make(map[string][]LockedFile),
		lockToast: make(map[string]time.Time),
		onToast:   func(title, msg, link string) { toasts = append(toasts, msg) },
	}
	ex := paths("a.xlsx")

	e.reconcileLocked("d", ex, []LockedFile{lf("a.xlsx", "bob")})
	if len(toasts) != 1 {
		t.Fatalf("first lock produced %d toasts, want 1", len(toasts))
	}

	e.reconcileLocked("d", ex, []LockedFile{lf("a.xlsx", "bob")})
	if len(toasts) != 1 {
		t.Fatalf("re-reporting the same lock toasted again: %d", len(toasts))
	}

	// Released and re-taken by the same person inside the window — still silent.
	e.reconcileLocked("d", ex, nil)
	e.reconcileLocked("d", ex, []LockedFile{lf("a.xlsx", "bob")})
	if len(toasts) != 1 {
		t.Fatalf("a flapping lock toasted again: %d", len(toasts))
	}

	// A different person is genuinely new.
	e.reconcileLocked("d", ex, []LockedFile{lf("a.xlsx", "carol")})
	if len(toasts) != 2 {
		t.Fatalf("a new lock holder produced %d toasts, want 2", len(toasts))
	}
}

// An APP lock has no person behind it: nc:lock-owner is absent and
// nc:lock-owner-displayname carries the APP's name. Verified live against a
// Nextcloud Text session, where displayname was "Text" — so the first cut of
// this feature would have said "Text is editing this in Nextcloud Office",
// inventing a colleague called Text and naming the wrong app.
func TestLockedFileSummaryWording(t *testing.T) {
	cases := []struct {
		name string
		f    LockedFile
		want string
	}{
		{"app lock names the app, not a person",
			LockedFile{Path: "notes/New notes.md", AppName: "Text", OwnerType: transport.LockOwnerApp},
			"New notes.md is open in Nextcloud Text"},
		{"office app lock",
			LockedFile{Path: "Budget.xlsx", AppName: "Office", OwnerType: transport.LockOwnerApp},
			"Budget.xlsx is open in Nextcloud Office"},
		{"manual lock names the person",
			LockedFile{Path: "a/Budget.xlsx", OwnerDisplay: "Bob Smith", Owner: "bob"},
			"Bob Smith has Budget.xlsx open"},
		{"manual lock with only a login",
			LockedFile{Path: "Budget.xlsx", Owner: "bob"},
			"bob has Budget.xlsx open"},
		{"unattributable lock stays vague",
			LockedFile{Path: "Budget.xlsx"},
			"Someone else has Budget.xlsx open"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.f.Summary(); got != c.want {
				t.Errorf("Summary() = %q, want %q", got, c.want)
			}
		})
	}

	// Who() must never hand back an app name as if it were a colleague.
	app := LockedFile{AppName: "Text", OwnerDisplay: "Text", OwnerType: transport.LockOwnerApp}
	if got := app.Who(); got == "Text" {
		t.Errorf("Who() = %q — an app name must not be presented as a person", got)
	}
}
