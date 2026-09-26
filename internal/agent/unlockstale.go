package agent

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/otherworld/nimbo/internal/transport"
)

// StaleLockAge is how old another person's lock has to be before the file's
// owner is offered Unlock. Long enough that it is not used on somebody who has
// the file open right now, which is what a lock usually means.
const StaleLockAge = time.Hour

// CanUnlock reports whether login may clear this lock from Nimbo (#733).
//
// Nextcloud lets a file's owner clear anyone's lock on it, and that is the only
// way a user on a hosted server without occ can free a lock the official client
// left behind. It is limited to a PERSON's lock: an app lock belongs to a live
// Text or Office session, and clearing it would break that session.
func (f LockedFile) CanUnlock(login string, now time.Time) bool {
	return login != "" && f.FileOwner != "" && strings.EqualFold(f.FileOwner, login) &&
		f.OwnerType == transport.LockOwnerUser && f.RemotePath != "" &&
		!f.Since.IsZero() && now.Sub(f.Since) >= StaleLockAge
}

// staleUnlocker is the part of the transport UnlockStale needs, so tests can
// stand in for the server.
type staleUnlocker interface {
	Stat(ctx context.Context, remotePath string) (transport.Entry, bool, error)
	Unlock(ctx context.Context, remotePath string) error
}

// UnlockStale clears another person's stale lock on a file this account owns.
// localDir and rel name the entry as LockedFiles lists it.
//
// This is deliberately not lockMgr.release, which refuses any lock we did not
// take. The file is read again first and the UNLOCK is only sent when it is
// still the same lock, so a lock taken a moment ago is never cleared.
func (e *Engine) UnlockStale(ctx context.Context, localDir, rel string) error {
	return e.unlockStale(ctx, e.client, localDir, rel, time.Now())
}

func (e *Engine) unlockStale(ctx context.Context, cl staleUnlocker, localDir, rel string, now time.Time) error {
	login := e.Account.LoginName
	f, ok := e.lockedEntry(localDir, rel)
	if !ok {
		return nil // already gone from the list: nothing left to clear
	}
	if !f.CanUnlock(login, now) {
		return errors.New("only the file's owner can clear a lock, and only one that has been there for over an hour")
	}
	ent, exists, err := cl.Stat(ctx, f.RemotePath)
	if err != nil {
		return err
	}
	only := map[string]bool{rel: true}
	if !exists || !ent.Lock.HeldByOther(login) {
		e.reconcileLocked(localDir, only, nil) // gone, or free already
		return nil
	}
	l := ent.Lock
	if !strings.EqualFold(l.Owner, f.Owner) || l.OwnerType != f.OwnerType || !l.Since.Equal(f.Since) {
		// Not the lock the user was shown: show the new one instead.
		e.reconcileLocked(localDir, only, []LockedFile{{
			Path: rel, LocalDir: localDir,
			Owner: l.Owner, OwnerDisplay: l.OwnerDisplay, AppName: l.AppName(),
			OwnerType: l.OwnerType, Since: l.Since,
			RemotePath: f.RemotePath, FileOwner: l.FileOwner,
		}})
		return errors.New("the lock has changed since the list was shown, someone may have just opened the file")
	}
	if err := cl.Unlock(ctx, f.RemotePath); err != nil {
		return err
	}
	slog.Info("cleared a stale lock on a file we own", "path", f.RemotePath,
		"by", f.Who(), "since", f.Since.Format(time.RFC3339))
	e.reconcileLocked(localDir, only, nil)
	if e.lockWarn != nil && e.lockoutEnabled() {
		e.lockWarn.apply(e.LockedFiles()) // let the user edit it straight away
	}
	return nil
}

// lockedEntry finds one entry of the locked set.
func (e *Engine) lockedEntry(localDir, rel string) (LockedFile, bool) {
	e.lockedMu.Lock()
	defer e.lockedMu.Unlock()
	for _, f := range e.locked[localDir] {
		if f.Path == rel {
			return f, true
		}
	}
	return LockedFile{}, false
}
