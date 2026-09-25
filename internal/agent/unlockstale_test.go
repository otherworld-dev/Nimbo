package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/otherworld/nimbo/internal/account"
	"github.com/otherworld/nimbo/internal/transport"
)

type fakeStaleClient struct {
	entry     transport.Entry
	exists    bool
	statErr   error
	unlocked  []string
	unlockErr error
}

func (f *fakeStaleClient) Stat(_ context.Context, p string) (transport.Entry, bool, error) {
	return f.entry, f.exists, f.statErr
}

func (f *fakeStaleClient) Unlock(_ context.Context, p string) error {
	if f.unlockErr != nil {
		return f.unlockErr
	}
	f.unlocked = append(f.unlocked, p)
	return nil
}

var staleNow = time.Unix(1790400000, 0)

// bobsOldLock is a person's lock, two hours old, on a file alice owns.
func bobsOldLock() LockedFile {
	return LockedFile{
		Path: "Team/Budget.xlsx", RemotePath: "Shared/Team/Budget.xlsx",
		Owner: "bob", OwnerType: transport.LockOwnerUser,
		Since: staleNow.Add(-2 * time.Hour), FileOwner: "alice",
	}
}

func staleEngine(f LockedFile) *Engine {
	e := &Engine{Account: account.Account{LoginName: "alice"}, locked: map[string][]LockedFile{}}
	e.locked[`C:\Sync`] = []LockedFile{f}
	return e
}

// The same lock, as a fresh Stat reports it.
func serverView(f LockedFile) transport.Entry {
	return transport.Entry{Path: f.RemotePath, Lock: &transport.LockInfo{
		Owner: f.Owner, OwnerType: f.OwnerType, Since: f.Since, FileOwner: f.FileOwner,
	}}
}

func TestCanUnlock(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*LockedFile)
		want bool
	}{
		{"owner, person's lock, two hours old", func(*LockedFile) {}, true},
		{"not the file's owner", func(f *LockedFile) { f.FileOwner = "carol" }, false},
		{"owner unknown", func(f *LockedFile) { f.FileOwner = "" }, false},
		{"an app's lock (Text, Office)", func(f *LockedFile) { f.OwnerType = transport.LockOwnerApp }, false},
		{"under an hour old", func(f *LockedFile) { f.Since = staleNow.Add(-59 * time.Minute) }, false},
		{"no lock time", func(f *LockedFile) { f.Since = time.Time{} }, false},
		{"saved before the remote path was kept", func(f *LockedFile) { f.RemotePath = "" }, false},
	}
	for _, c := range cases {
		f := bobsOldLock()
		c.mod(&f)
		if got := f.CanUnlock("alice", staleNow); got != c.want {
			t.Errorf("%s: CanUnlock = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestUnlockStaleClearsTheLock(t *testing.T) {
	f := bobsOldLock()
	e := staleEngine(f)
	cl := &fakeStaleClient{entry: serverView(f), exists: true}
	if err := e.unlockStale(context.Background(), cl, `C:\Sync`, f.Path, staleNow); err != nil {
		t.Fatal(err)
	}
	if len(cl.unlocked) != 1 || cl.unlocked[0] != "Shared/Team/Budget.xlsx" {
		t.Errorf("unlocked = %v, want the remote path", cl.unlocked)
	}
	if n := len(e.LockedFiles()); n != 0 {
		t.Errorf("%d entries still listed after the unlock", n)
	}
}

func TestUnlockStaleRefusesWhatItMayNotClear(t *testing.T) {
	f := bobsOldLock()
	f.Since = staleNow.Add(-10 * time.Minute)
	e := staleEngine(f)
	cl := &fakeStaleClient{entry: serverView(f), exists: true}
	if err := e.unlockStale(context.Background(), cl, `C:\Sync`, f.Path, staleNow); err == nil {
		t.Fatal("a ten-minute-old lock was cleared")
	}
	if len(cl.unlocked) != 0 {
		t.Errorf("UNLOCK sent: %v", cl.unlocked)
	}
}

// Between the list being shown and the click, bob may have closed the file and
// opened it again. That new lock is not stale, and must be left alone.
func TestUnlockStaleLeavesANewerLockAlone(t *testing.T) {
	f := bobsOldLock()
	e := staleEngine(f)
	fresh := f
	fresh.Since = staleNow.Add(-time.Minute)
	cl := &fakeStaleClient{entry: serverView(fresh), exists: true}
	if err := e.unlockStale(context.Background(), cl, `C:\Sync`, f.Path, staleNow); err == nil {
		t.Fatal("cleared a lock taken a minute ago")
	}
	if len(cl.unlocked) != 0 {
		t.Errorf("UNLOCK sent: %v", cl.unlocked)
	}
	// The list now shows the new lock.
	got := e.LockedFiles()
	if len(got) != 1 || !got[0].Since.Equal(fresh.Since) {
		t.Errorf("listed = %+v, want the new lock", got)
	}
}

// Already free on the server: nothing to send, and the entry goes.
func TestUnlockStaleWhenAlreadyFree(t *testing.T) {
	f := bobsOldLock()
	e := staleEngine(f)
	cl := &fakeStaleClient{entry: transport.Entry{Path: f.RemotePath}, exists: true}
	if err := e.unlockStale(context.Background(), cl, `C:\Sync`, f.Path, staleNow); err != nil {
		t.Fatal(err)
	}
	if len(cl.unlocked) != 0 {
		t.Errorf("UNLOCK sent for a free file: %v", cl.unlocked)
	}
	if n := len(e.LockedFiles()); n != 0 {
		t.Errorf("%d entries still listed", n)
	}
}

// A failed check or UNLOCK keeps the entry, so the list does not claim a lock
// is gone when it is not.
func TestUnlockStaleKeepsTheEntryOnFailure(t *testing.T) {
	f := bobsOldLock()
	for name, cl := range map[string]*fakeStaleClient{
		"stat":   {statErr: errors.New("offline")},
		"unlock": {entry: serverView(f), exists: true, unlockErr: errors.New("423 Locked")},
	} {
		e := staleEngine(f)
		if err := e.unlockStale(context.Background(), cl, `C:\Sync`, f.Path, staleNow); err == nil {
			t.Errorf("%s: no error", name)
		}
		if n := len(e.LockedFiles()); n != 1 {
			t.Errorf("%s: %d entries listed, want the lock kept", name, n)
		}
	}
}
