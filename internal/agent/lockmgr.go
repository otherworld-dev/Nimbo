package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/otherworld/nimbo/internal/config"
	"github.com/otherworld/nimbo/internal/officelock"
	"github.com/otherworld/nimbo/internal/transport"
)

// locker is the slice of transport.Client the lock manager needs. An interface
// so the lifetime logic can be tested without a server — the part that must not
// be wrong is exactly the part that is awkward to reach over the network.
type locker interface {
	Lock(ctx context.Context, remotePath string) (transport.LockResult, error)
	Unlock(ctx context.Context, remotePath string) error
}

// lockMgr owns every lock Nimbo takes, from acquisition to release.
//
// It exists because a default Nextcloud NEVER expires a lock and no other user
// can clear one (UNLOCK by a non-owner is 423). We watched a Nextcloud Text
// session strand a lock indefinitely on 2026-08-09; if Nimbo does the same to a
// colleague's file, nothing will ever fix it. So every lock is written to a
// registry on disk BEFORE it is taken, refreshed while held, released on close
// and on shutdown, and swept at the next startup if a run died holding one.
type lockMgr struct {
	cl      locker
	dirs    config.Dirs
	account string

	mu   sync.Mutex
	held map[string]config.HeldLock // remote path -> our lock
	now  func() time.Time           // injectable for tests
}

func newLockMgr(cl locker, dirs config.Dirs, account string) *lockMgr {
	return &lockMgr{
		cl: cl, dirs: dirs, account: account,
		held: map[string]config.HeldLock{},
		now:  time.Now,
	}
}

// persistLocked rewrites the registry from our in-memory set, preserving every
// entry that belongs to a DIFFERENT account. Call with mu held.
func (m *lockMgr) persistLocked() {
	onDisk, _ := m.dirs.LoadHeldLocks()
	out := make([]config.HeldLock, 0, len(onDisk)+len(m.held))
	for _, l := range onDisk {
		if l.Account != m.account {
			out = append(out, l) // another account's business
		}
	}
	for _, l := range m.held {
		out = append(out, l)
	}
	if err := m.dirs.SaveHeldLocks(out); err != nil {
		// Not fatal: the lock is still real and we still hold it in memory, so
		// this run will release it normally. Only a crash would now strand it.
		slog.Warn("could not record held locks; a crash could strand one", "err", err)
	}
}

// take locks remotePath on the server and records it.
//
// The record is written BEFORE the LOCK deliberately. A crash between the two
// leaves a phantom entry, which the next sweep tries to release and the server
// answers 412 — harmless. The other order would leave a real lock with no
// record, which nothing could ever clean up.
func (m *lockMgr) take(ctx context.Context, remotePath string) error {
	m.mu.Lock()
	if _, already := m.held[remotePath]; already {
		m.mu.Unlock()
		return m.refreshOne(ctx, remotePath)
	}
	m.held[remotePath] = config.HeldLock{
		Account: m.account, RemotePath: remotePath, Taken: m.now(),
	}
	m.persistLocked()
	m.mu.Unlock()

	res, err := m.cl.Lock(ctx, remotePath)
	if err != nil {
		m.mu.Lock()
		delete(m.held, remotePath)
		m.persistLocked()
		m.mu.Unlock()
		return err
	}
	m.mu.Lock()
	l := m.held[remotePath]
	l.Token = res.Token
	m.held[remotePath] = l
	m.persistLocked()
	m.mu.Unlock()
	slog.Info("took a lock", "path", remotePath, "token", res.Token)
	return nil
}

// refreshOne re-LOCKs a path we already hold. Re-locking your own lock returns
// 200 with the same token — that is the whole refresh mechanism; files_lock has
// no separate verb.
func (m *lockMgr) refreshOne(ctx context.Context, remotePath string) error {
	_, err := m.cl.Lock(ctx, remotePath)
	return err
}

// holds reports whether we hold remotePath's lock (or are taking it).
func (m *lockMgr) holds(remotePath string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.held[remotePath]
	return ok
}

// release unlocks a path and forgets it. The UNLOCK comes first: if it fails we
// keep the record, so a later sweep can try again. Forgetting first would strand
// a real lock on a transient error.
func (m *lockMgr) release(ctx context.Context, remotePath string) error {
	m.mu.Lock()
	_, ours := m.held[remotePath]
	m.mu.Unlock()
	if !ours {
		// Never UNLOCK something we did not take: it is not ours to clear, and
		// the server answers 423.
		return nil
	}
	if err := m.cl.Unlock(ctx, remotePath); err != nil {
		slog.Warn("could not release a lock; will retry on the next sweep", "path", remotePath, "err", err)
		return err
	}
	m.mu.Lock()
	delete(m.held, remotePath)
	m.persistLocked()
	m.mu.Unlock()
	slog.Info("released a lock", "path", remotePath)
	return nil
}

// releaseAll drops every lock this account holds, returning how many went. Used
// by shutdown and by the user's escape hatch.
func (m *lockMgr) releaseAll(ctx context.Context) (int, error) {
	m.mu.Lock()
	paths := make([]string, 0, len(m.held))
	for p := range m.held {
		paths = append(paths, p)
	}
	m.mu.Unlock()

	var firstErr error
	n := 0
	for _, p := range paths {
		if err := m.release(ctx, p); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		n++
	}
	return n, firstErr
}

// heartbeat re-LOCKs everything we hold, so a server configured WITH a
// lock_timeout does not expire a file somebody still has open. On a server
// without one it is a cheap no-op that also proves the lock is still ours.
func (m *lockMgr) heartbeat(ctx context.Context) {
	m.mu.Lock()
	paths := make([]string, 0, len(m.held))
	for p := range m.held {
		paths = append(paths, p)
	}
	m.mu.Unlock()

	for _, p := range paths {
		if ctx.Err() != nil {
			return
		}
		if err := m.refreshOne(ctx, p); err != nil {
			if transport.IsLocked(err) {
				// Somebody else holds it now, so it is not ours to refresh or to
				// release. Drop the record rather than fight over it.
				slog.Warn("a lock we held is now someone else's; forgetting it", "path", p)
				m.mu.Lock()
				delete(m.held, p)
				m.persistLocked()
				m.mu.Unlock()
				continue
			}
			slog.Warn("lock heartbeat failed", "path", p, "err", err)
		}
	}
}

// sweep releases locks a PREVIOUS run left behind, and forgets them either way.
//
// It must run before this process takes any locks of its own. Entries belonging
// to other accounts are left untouched. A 412 from the server means the lock is
// already gone, which Unlock treats as success — so re-sweeping is silent rather
// than logging a failure on every boot.
func (m *lockMgr) sweep(ctx context.Context) (int, error) {
	onDisk, _ := m.dirs.LoadHeldLocks()
	var mine, others []config.HeldLock
	for _, l := range onDisk {
		if l.Account == m.account {
			mine = append(mine, l)
		} else {
			others = append(others, l)
		}
	}
	if len(mine) == 0 {
		return 0, nil
	}

	n := 0
	var firstErr error
	var stuck []config.HeldLock
	for _, l := range mine {
		if err := m.cl.Unlock(ctx, l.RemotePath); err != nil {
			slog.Warn("could not release a lock left by a previous run",
				"path", l.RemotePath, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			if !transport.IsLocked(err) {
				stuck = append(stuck, l) // keep trying next boot
			}
			continue
		}
		n++
		slog.Info("released a lock left by a previous run", "path", l.RemotePath)
	}

	m.mu.Lock()
	for _, l := range stuck {
		m.held[l.RemotePath] = l
	}
	_ = others // preserved by persistLocked, which re-reads the file
	m.persistLocked()
	m.mu.Unlock()
	return n, firstErr
}

// documentForLockFile maps an editor's lock file back to the document it belongs
// to. dirNames is the listing of the folder it sits in, needed because the Word
// owner-file mapping is many-to-one and cannot be inverted by string surgery.
func documentForLockFile(base string, dirNames []string) (string, bool) {
	if doc, ok := officelock.DocumentFromLibreLock(base); ok {
		return doc, true // exact — LibreOffice does not truncate
	}
	if officelock.IsOwnerFile(base) {
		return officelock.Document(base, dirNames)
	}
	return "", false
}

// handleEditorLockFiles turns "Word just created ~$Report.docx" into a lock on
// the server, and its disappearance into a release.
//
// It runs on the raw watcher paths, before relsFor and the ignore filter drop
// them — this is the last point at which those events exist. Nothing here may
// fail a sync: a lock we cannot take is logged and forgotten.
func (e *Engine) handleEditorLockFiles(ctx context.Context, p Pair, changed []string) {
	if len(changed) == 0 || !e.LockingAvailable() || !e.lockingEnabled() {
		return
	}
	// A one-way backup pair never writes to the server, and TakeLock below is a
	// write — a WebDAV LOCK the server holds until we release it. Locking exists
	// so two people editing the same document see each other; in a backup folder
	// the local edit is discarded by the next pass anyway, so the lock buys
	// nothing and a crash would strand it on the server's copy.
	//
	// While the guard state is unreadable the account is suspended and no sync
	// runs — a lock taken here would be held with nothing behind it, and on a
	// default Nextcloud nothing ever expires it (see the type comment above).
	if e.guardStateUnavailable() {
		return
	}
	e.editorLockMu.Lock()
	defer e.editorLockMu.Unlock()
	esc := e.escaper.Load()
	for _, abs := range changed {
		base := filepath.Base(abs)
		if !officelock.IsOwnerFile(base) && !strings.HasPrefix(base, ".~lock.") {
			continue
		}
		// The lockout writes owner files of its own beside a colleague's
		// document. Those say "someone ELSE has it open", not that the user
		// opened it; locking on them would try to take the colleague's file.
		if e.lockWarn != nil && e.lockWarn.isSynth(abs) {
			continue
		}
		dir := filepath.Dir(abs)
		var names []string
		if ents, err := os.ReadDir(dir); err == nil {
			for _, en := range ents {
				names = append(names, en.Name())
			}
		}
		doc, ok := documentForLockFile(base, names)
		if !ok {
			// Ambiguous or unmatched: refusing beats locking a document the user
			// never opened. Word's owner name genuinely collides between siblings.
			slog.Debug("editor lock file did not resolve to one document", "file", abs)
			continue
		}
		rel, err := filepath.Rel(p.LocalDir, filepath.Join(dir, doc))
		if err != nil {
			continue
		}
		rel = filepath.ToSlash(rel)
		if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
			continue
		}
		// The server stores an escaped name for a forbidden one, so LOCK must
		// target the ENCODED path — the same sidedness bug that produced #550.
		remote := strings.Trim(p.RemoteRoot+"/"+esc.Encode(rel), "/")

		if _, statErr := os.Lstat(abs); statErr == nil {
			// Already ours: one open is one LOCK. Word's owner file changes
			// more than once while it opens, and on the live server repeated
			// LOCKs in quick succession left a lock behind that the UNLOCK at
			// close did not clear (VM, 2026-09-23). The heartbeat refreshes it.
			if e.lockMgr.holds(remote) {
				continue
			}
			if err := e.TakeLock(ctx, remote); err != nil {
				if transport.IsLocked(err) {
					// Somebody got there first. Entirely normal contention — the
					// warning half of the feature has already told the user.
					slog.Info("did not lock a file just opened: someone else holds it", "path", remote)
				} else {
					slog.Warn("could not lock a file that was just opened", "path", remote, "err", err)
				}
			}
			continue
		}
		if err := e.ReleaseLock(ctx, remote); err != nil {
			slog.Warn("could not release the lock on a file that was closed", "path", remote, "err", err)
		}
	}
}

// NoteEditorLockFiles is the on-demand entry to handleEditorLockFiles: the
// watcher of an on-demand mount hands over the editor lock files it saw come
// and go, and the mount folder and its server root stand in for a sync pair's
// (on-demand mode has no pairs; Deck #721).
func (e *Engine) NoteEditorLockFiles(ctx context.Context, mountDir, remoteRoot string, absPaths []string) {
	e.handleEditorLockFiles(ctx, Pair{LocalDir: mountDir, RemoteRoot: strings.Trim(remoteRoot, "/")}, absPaths)
}

// ReleaseLockoutHandle lets go of the deny-write handle the lockout holds on
// abs, keeping the warning itself. The on-demand watcher calls it just before
// it dehydrates a downloaded file (a refresh, or "Free up space"), which the
// handle would otherwise make fail. A path with no handle is a no-op.
func (e *Engine) ReleaseLockoutHandle(abs string) {
	if e.lockWarn != nil {
		e.lockWarn.release(abs)
	}
}

// NoteRemoteLocks feeds lock state from an on-demand directory listing into the
// same set the live-sync path maintains, so the "In use" list and its toasts
// work in virtual-files mode too.
//
// On-demand mode needs its own entry point because it has no sync pairs at all:
// Engine.Run gets none, so applyPlan — where the live path does this — never
// executes. The mount directory stands in for a pair's local root.
//
// It is called per directory as the shell populates it, and reconciles only the
// paths in THAT listing, so other folders' locks are left untouched.
func (e *Engine) NoteRemoteLocks(mountDir, remoteRoot string, entries []transport.Entry) {
	if !e.LockingAvailable() || e.lockMgr == nil {
		return
	}
	root := strings.Trim(remoteRoot, "/")
	examined := make(map[string]bool, len(entries))
	var locked []LockedFile
	for _, en := range entries {
		if en.IsDir {
			continue
		}
		rel := strings.Trim(en.Path, "/")
		if root != "" {
			if !strings.HasPrefix(rel, root+"/") {
				continue
			}
			rel = strings.TrimPrefix(rel, root+"/")
		}
		if rel == "" {
			continue
		}
		examined[rel] = true
		if !en.Lock.HeldByOther(e.Account.LoginName) {
			continue
		}
		locked = append(locked, LockedFile{
			Path: rel, LocalDir: mountDir,
			Owner: en.Lock.Owner, OwnerDisplay: en.Lock.OwnerDisplay,
			AppName: en.Lock.AppName(), OwnerType: en.Lock.OwnerType, Since: en.Lock.Since,
		})
	}
	if len(examined) == 0 {
		return
	}
	e.reconcileLocked(mountDir, examined, locked)
	// The listing is where on-demand mode sees a colleague's lock come and go,
	// so it drives the lockout as applyPlan does for live sync, with the full
	// set: apply undoes whatever is missing from what it is given.
	if e.lockWarn != nil && e.lockoutEnabled() {
		e.lockWarn.apply(e.LockedFiles())
	}
}

// lockoutEnabled reports whether we may hold other people's locked files open
// locally, so their editor refuses to edit them.
//
// Gated separately from lockingEnabled because it is the intrusive half: taking
// a lock inconveniences colleagues, but holding a handle stops the person at
// this keyboard editing a document, and a bug in the release path stops them
// until Nimbo restarts.
func (e *Engine) lockoutEnabled() bool {
	s, err := e.dirs.LoadSettings()
	if err != nil {
		return false
	}
	return s.FileLockout
}

// lockingEnabled reports the user's setting. Off by default: taking a lock is
// visible to colleagues and, on a server with no lock_timeout, a bug here would
// strand a file that nobody but an admin could free.
func (e *Engine) lockingEnabled() bool {
	s, err := e.dirs.LoadSettings()
	if err != nil {
		return false
	}
	return s.FileLocking
}

// list returns the locks we currently hold.
func (m *lockMgr) list() []config.HeldLock {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]config.HeldLock, 0, len(m.held))
	for _, l := range m.held {
		out = append(out, l)
	}
	return out
}
