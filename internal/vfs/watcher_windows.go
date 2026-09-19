//go:build windows

// Package vfs provides write-back for on-demand (Cloud Files) folders: it
// watches a mounted sync root for user changes and pushes them to the server
// (uploads, folder creates, deletes, renames), then marks placeholders in-sync
// so its own writes aren't re-processed. This is separate from the two-way diff
// engine, which must never scan placeholder folders.
package vfs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/otherworld/nimbo/internal/cfapi"
	"github.com/otherworld/nimbo/internal/transport"
)

// cfapi seams — package-level function vars so tests can substitute an
// in-memory placeholder world for the live Cloud Files API. Production code
// always uses the real implementations.
var (
	cfInspect             = cfapi.Inspect
	cfCreatePlaceholders  = cfapi.CreatePlaceholders
	cfRefreshPlaceholder  = cfapi.RefreshPlaceholder
	cfRefreshIfInSync     = cfapi.RefreshPlaceholderIfInSync
	cfUpdateIdentity      = cfapi.UpdateIdentity
	cfUpdateIdentityKeep  = cfapi.UpdateIdentityKeepState
	cfMarkInSync          = cfapi.MarkInSync
	cfSetInSync           = cfapi.SetInSync
	cfShellNotify         = cfapi.ShellNotifyUpdated
	cfShellCreated        = cfapi.ShellNotifyCreated
	cfSettlePin           = cfapi.SettlePin
	cfExclude             = cfapi.ExcludeFromSync
	cfPlaceholderIdentity = cfapi.PlaceholderIdentity
	cfPlaceholderModified = cfapi.PlaceholderModified
	cfHydrateIfPinned     = cfapi.HydrateIfPinned
	cfPinnedDehydrated    = cfapi.PinnedDehydrated
	cfDirPopulated        = cfapi.DirPopulated
)

// Ops are the server-side actions the watcher performs (backed by the engine).
type Ops struct {
	Upload func(ctx context.Context, localPath, remotePath string) error
	Mkdir  func(ctx context.Context, remotePath string) error
	Delete func(ctx context.Context, remotePath string) error
	Move   func(ctx context.Context, srcRemote, dstRemote string) error
	// List returns the children of a sync-root-relative directory ("" = root)
	// as placeholders, for down-sync reconciliation. A non-nil error means the
	// listing is unknown (e.g. a network failure) and must NOT be treated as an
	// empty directory.
	List func(rel string) ([]cfapi.PlaceholderInfo, error)
	// CheckList lists a server folder for the delete guard, sync-root-relative
	// like List, but completely and quietly: end-to-end encrypted folders are
	// included (Encrypted set) rather than skipped, nothing is recorded as a
	// side effect, and a listing that cannot finish returns an error (a
	// transport.ErrNotFound one when the folder is not there). Nil makes the
	// guard use List.
	CheckList func(rel string) ([]cfapi.PlaceholderInfo, error)
	// Stat reports whether a RAW server path currently exists — used to tell a
	// lost MOVE response (the server applied the rename) from a real failure.
	// Nil disables that detection.
	Stat func(remote string) (bool, error)
	// Report surfaces a completed operation for the activity feed / error toasts
	// (kind e.g. "upload"/"delete-remote"/"move"/"delete-local"; err non-nil on
	// failure).
	Report func(kind, remotePath string, err error)
	// RecordBaseline records the server ETag a placeholder now mirrors (the
	// conflict baseline), set when we create/refresh an in-sync placeholder.
	RecordBaseline func(remotePath, etag string)
	// Baseline returns the recorded ETag for a remote path (ok=false if none).
	// Down-sync uses it to detect server-side edits reliably (any content change
	// alters the ETag), rather than relying only on the size/mtime heuristic.
	Baseline func(remotePath string) (string, bool)
	// ForgetBaseline drops the recorded ETag for a remote path the server no
	// longer holds under that name (the source of a MOVE). Nil disables it.
	ForgetBaseline func(remotePath string)
	// MoveBaselines carries several baselines across a move at once (pairs of
	// src, dst RAW server paths): each dst takes src's recorded ETag and src is
	// dropped. RecordBaselines records several baselines at once.
	//
	// Both exist because the store behind these hooks persists its ENTIRE file
	// per call: a moved directory of 1,000 files cost 2,000 whole-file rewrites
	// of a multi-megabyte JSON, synchronously, inside the move. Nil falls back
	// to the one-item hooks above, so a caller wiring only those still works.
	MoveBaselines   func(pairs [][2]string)
	RecordBaselines func(etagByRemotePath map[string]string)
	// RecordFileID / FileID / DropFileID persist the server oc:fileid per remote
	// path so down-sync can recognise a server rename (old path gone, new path
	// with the same fileid) and move the placeholder instead of delete+recreate.
	RecordFileID func(remotePath, fileid string)
	FileID       func(remotePath string) (string, bool)
	DropFileID   func(remotePath string)
	// MountRoot reports whether a RAW server path was, when last listed, the
	// ROOT of a share received from someone else or of a mount (Deck #557).
	// Such a path vanishing from a listing means it was detached from the
	// account, not deleted. Nil disables the distinction: every vanish is a
	// deletion, as before.
	MountRoot func(remotePath string) bool
	// Detached takes over a vanished share's salvaged local copy at localPath
	// (plain files only by then — see Watcher.salvage), keyed by its RAW server
	// path: the caller parks it outside the cloud folder and tells the user.
	// An error leaves the copy where it is for the next pass to retry.
	Detached func(localPath, remotePath string) error
	// Forget is told, once a vanished share's copy has left the mount (parked)
	// or was removed as holding nothing, that whatever is recorded under
	// remotePath — etag baselines, file ids, the root mark — no longer applies.
	// Seen live: the stub-only share's stale entries would have let the
	// mount-state heal read a later folder of the same name as server content,
	// and its stale root mark would then have parked that folder as "unshared".
	Forget func(remotePath string)
	// Encode maps a LOCAL (user-visible) rel path to the RAW path the server
	// stores it under, for disguised file types: Nextcloud forbids ".htaccess",
	// so it lives on the server as ".htaccess.nimboesc". Decode is the inverse and
	// is used only for display. Nil means escaping is off and names pass through.
	//
	// Escaping applies to FILE basenames only — directories are never escaped, so
	// callers must not encode a directory path (see serverFor).
	//
	// These are called PER OPERATION and must never be captured by the caller: the
	// engine swaps its escaper (an atomic pointer) whenever the user toggles a
	// type in Settings, and a mount outlives that.
	Encode func(rel string) string
	Decode func(rel string) string
	Log    func(format string, args ...any)
}

// Watcher monitors a mount root subtree and pushes user changes to the server.
type Watcher struct {
	root       string // local sync-root path
	remoteRoot string // files-root-relative remote path ("" = account root)
	ops        Ops
	handle     windows.Handle
	ctx        context.Context
	cancel     context.CancelFunc

	pollEvery time.Duration // safety-net reconcile interval

	mu       sync.Mutex
	upload   map[string]*time.Timer // debounced uploads (coalesce write bursts)
	delete   map[string]*time.Timer // debounced deletes (cancelled if path returns)
	suppress map[string]time.Time   // paths we removed ourselves (skip server delete)
	// inflight holds a cancel func per lower-cased path with an upload running
	// now — a delete/rename event for the path cancels the doomed upload
	// instead of letting the assembly recreate a deleted file server-side.
	inflight    map[string]context.CancelFunc
	again       map[string]bool // changed while in flight — re-arm on completion
	attempts    map[string]int  // consecutive upload failures per lower-cased path
	delAttempts map[string]int  // consecutive server-DELETE failures
	// deleting holds the paths whose delete is being judged or sent right now,
	// and kept the folders the delete guard kept on the server recently (both
	// lower-cased); guardSem bounds how many guard listings run at once. All
	// three are created on first use.
	deleting   map[string]int
	kept       map[string]time.Time
	guardSem   chan struct{}
	mvAttempts map[string]int // consecutive MOVE failures per destination
	busyCount  map[string]int // consecutive local-sharing-violation retries
	// moved holds two kinds of keys, both cleaned up as soon as their purpose
	// is served (their TTLs — see movedKeyTTL — are only a backstop): a pair
	// key (lower(old)+"\x00"+lower(new)) claims a rename so that when the same
	// rename is reported twice — the RENAMED pair and the filter's
	// rename-completion callback — only one MOVE is pushed; a lower-cased
	// SOURCE path is recorded only when that move's own REMOVED hasn't
	// arrived yet (Windows reports a cross-directory move as REMOVED + ADDED),
	// and is consumed by the one REMOVED it exists for — so it never lingers
	// to suppress an unrelated, later, genuine delete of a new file created at
	// the vacated path (issue #7).
	moved     map[string]time.Time
	pokeTimer *time.Timer // debounced push-triggered reconcile
	// Pin hydration: requests wait in hydratePending for one of the
	// hydrateWorkers drainers, so a recursively pinned tree costs a path
	// string per file rather than a parked goroutine. hydrateWake (capacity
	// 1) is the nudge that tells a sleeping worker there is something to
	// take. hydrating holds every path currently pending OR downloading, so
	// the same file is never requested twice.
	hydratePending []string
	hydrateWake    chan struct{}
	hydrating      map[string]bool
	// hydrateFails/hydrateNext back off a repeatedly-failing hydration (a
	// server outage, a file the server has since deleted) so reconcile
	// doesn't retry and re-report it on every pass; both keyed lower-cased,
	// both guarded by mu like the maps above.
	hydrateFails map[string]int
	hydrateNext  map[string]time.Time
	// inMove holds lower-cased DESTINATION paths with a server MOVE running
	// right now. Reconcile's foreign-identity check reads the same evidence a
	// live move does (an identity naming another server path), so without this
	// a pass landing mid-move issues a second MOVE for the same file; the
	// loser 404s, which surfaces as a bogus "missing on the server" error for
	// an online-only file or a redundant full re-upload for a hydrated one.
	inMove map[string]bool
	// verifying holds lower-cased paths with a deferred placement check
	// pending (verifyPlacement). A rename the pair claim deduplicates is
	// usually a second report of one move, so several may arrive for the same
	// path; one check answers them all, and two racing each other would issue
	// competing MOVEs.
	verifying map[string]bool
	// forced holds lower-cased paths whose next handleChange must run its
	// Mkdir/Upload branch even though cfInspect reports NeedsUpload == false —
	// moveServer's 404 fallback for a copy that already has data (or a
	// directory), which a plain handleChange would otherwise silently skip.
	forced map[string]bool

	loopDone chan struct{} // closed when the watch loop exits (Close joins it)

	lostEvents atomic.Bool // events were lost (overflow/reopen) — next pass sweeps

	reconMu sync.Mutex // serialises reconcile passes
	// firstPassDone and fullSweep are guarded by reconMu (only reconcile
	// passes touch them):
	firstPassDone bool // a full (skip-free) pass has completed since start
	fullSweep     bool // the running pass ignores the ETag subtree skip
	// corruptReported remembers entries already reported by noteCorrupt (lower-
	// cased local paths), so a permanently broken entry is announced once. It
	// is guarded by w.mu, NOT reconMu: noteCorrupt is callable from anywhere.
	corruptReported map[string]bool
	// moveWaitNoted (lower-cased local paths, guarded by mu) marks changes
	// currently waiting on a move whose first wait has been logged.
	moveWaitNoted map[string]bool
}

const (
	uploadDebounce = 800 * time.Millisecond
	// Deletes wait a beat so a delete that's really part of a rename/atomic-save
	// (the path reappears) can be cancelled before it hits the server.
	deleteDebounce = 1200 * time.Millisecond
	// pokeDebounce coalesces a burst of notify_push events into one reconcile.
	pokeDebounce = 1500 * time.Millisecond
	// movedSourceTTL bounds how long a move's SOURCE path is remembered; a
	// REMOVED for it within this window is the move's own event, not a
	// deletion. It is short on purpose: that REMOVED arrives within
	// milliseconds of the move and its delete timer fires at 1.2 s, so a
	// source key still unconsumed after this long is not waiting for anything
	// — it is what a second report of the same move left behind (the RENAMED
	// pair and the filter's callback both do the delete bookkeeping, and only
	// one of them has a REMOVED to account for). Left at two minutes, that
	// leftover swallowed a genuine delete of a NEW file created at the vacated
	// path — which reconcile then pulled back down — on essentially every
	// dual-reported rename.
	movedSourceTTL = 15 * time.Second
	// renameClaimTTL bounds how long a PAIR key claims one rename, so the two
	// reporters of a single move (the RENAMED pair and the filter's
	// rename-completion callback, milliseconds apart) push it once. The same
	// move made again later — the user undoes a move and redoes it — is a NEW
	// rename that must be pushed, not a duplicate report of the first.
	renameClaimTTL = 15 * time.Second
	// renameVerifyPoll paces verifyPlacement's wait for a MOVE the first
	// claimant of a deduplicated rename is already pushing, and
	// renameVerifyMax bounds that wait: past it the retry chain that still
	// holds the mark is finishing the job on its own, and a second MOVE now
	// would only 404.
	renameVerifyPoll = 250 * time.Millisecond
	renameVerifyMax  = 60 * time.Second
	// hydrateWorkers bounds how many pinned files download at once. Any
	// number may be waiting their turn: a waiting request costs one path
	// string, and dropping one loses the file for good (see requestHydration).
	hydrateWorkers = 2
)

// renameVerifyDelay is how long verifyPlacement waits before it looks at where
// a deduplicated rename report's item ended up: the first claimant's
// handleRename goroutine must have reached moveServer and set inMove before we
// look, or the two read the same evidence and race — one of the two MOVEs
// 404s. A var, not a const, so tests need not sleep through a whole second.
var renameVerifyDelay = time.Second

// New starts a write-back watcher over root (mapped to remoteRoot). pollEvery is
// the safety-net reconcile interval (downsync mainly runs via Poke on push).
// Call Close to stop it.
func New(parent context.Context, root, remoteRoot string, pollEvery time.Duration, ops Ops) (*Watcher, error) {
	pathW, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(pathW,
		windows.FILE_LIST_DIRECTORY,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	if pollEvery <= 0 {
		pollEvery = 30 * time.Second
	}
	w := &Watcher{
		root: root, remoteRoot: strings.Trim(remoteRoot, "/"), ops: ops, pollEvery: pollEvery,
		handle: h, ctx: ctx, cancel: cancel,
		upload: map[string]*time.Timer{}, delete: map[string]*time.Timer{},
		suppress: map[string]time.Time{},
		inflight: map[string]context.CancelFunc{}, again: map[string]bool{},
		attempts: map[string]int{}, delAttempts: map[string]int{},
		mvAttempts: map[string]int{}, busyCount: map[string]int{},
		moved:       map[string]time.Time{},
		inMove:      map[string]bool{},
		verifying:   map[string]bool{},
		forced:      map[string]bool{},
		loopDone:    make(chan struct{}),
		hydrateWake: make(chan struct{}, 1), hydrating: map[string]bool{},
		hydrateFails: map[string]int{}, hydrateNext: map[string]time.Time{},
	}
	if w.ops.Log == nil {
		w.ops.Log = func(string, ...any) {}
	}
	for i := 0; i < hydrateWorkers; i++ {
		go w.hydrateLoop()
	}
	go w.loop()
	if w.ops.List != nil {
		go w.pollLoop()
		// pollLoop waits a whole interval before its first pass (30s, or five
		// minutes where push does the work), and nothing else pokes a freshly
		// mounted watcher — so after every start the shell's own root fetch
		// won the race and no pass had run at all for that interval. Poke
		// rather than Reconcile directly: the startup burst of push events
		// coalesces into the same first pass instead of queueing behind it.
		w.Poke()
	}
	return w, nil
}

// Poke requests a reconcile soon, coalescing a burst of triggers (e.g. several
// notify_push events) into a single pass. Safe to call from any goroutine.
func (w *Watcher) Poke() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pokeTimer == nil {
		w.pokeTimer = time.AfterFunc(pokeDebounce, w.Reconcile)
		return
	}
	w.pokeTimer.Reset(pokeDebounce)
}

// pollLoop periodically reconciles populated directories with the server as a
// safety net (push-driven Poke handles the common case).
func (w *Watcher) pollLoop() {
	t := time.NewTicker(w.pollEvery)
	defer t.Stop()
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-t.C:
			w.Reconcile()
		}
	}
}

// Close stops the watcher and waits for the watch loop to exit, so the
// directory handle can't be closed (and its value recycled) under a loop
// about to issue another read on it.
func (w *Watcher) Close() {
	w.cancel()
	w.mu.Lock()
	h := w.handle
	w.mu.Unlock()
	windows.CancelIoEx(h, nil)
	windows.CloseHandle(h)
	if w.loopDone != nil {
		select {
		case <-w.loopDone:
		case <-time.After(5 * time.Second):
		}
	}
}

// FILE_NOTIFY_INFORMATION action codes.
const (
	fileActionAdded      = 0x1
	fileActionRemoved    = 0x2
	fileActionModified   = 0x3
	fileActionRenamedOld = 0x4
	fileActionRenamedNew = 0x5
	// ATTRIBUTES is load-bearing for the pin contract: Explorer's "Free up
	// space" / "Always keep" verbs change only the PINNED/UNPINNED attributes,
	// and without this flag the watcher never hears about them.
	fileNotifyChangeFlags = windows.FILE_NOTIFY_CHANGE_FILE_NAME |
		windows.FILE_NOTIFY_CHANGE_DIR_NAME |
		windows.FILE_NOTIFY_CHANGE_ATTRIBUTES |
		windows.FILE_NOTIFY_CHANGE_SIZE |
		windows.FILE_NOTIFY_CHANGE_LAST_WRITE
)

func (w *Watcher) loop() {
	defer close(w.loopDone)
	buf := make([]byte, 64*1024)
	for {
		var n uint32
		w.mu.Lock()
		h := w.handle
		w.mu.Unlock()
		err := windows.ReadDirectoryChanges(h, &buf[0], uint32(len(buf)),
			true, // watch the whole subtree — covers lazily-populated subdirs
			fileNotifyChangeFlags, &n, nil, 0)
		if w.ctx.Err() != nil {
			return
		}
		if err != nil {
			// The watch died (handle invalidated, transient kernel error) but
			// the mount is still live — a dead watcher silently stops ALL
			// write-back. Re-open and carry on; events in the gap are covered
			// by the full-sweep rescue below.
			w.ops.Log("vfs watch error: %v (reopening)", err)
			if !w.reopenHandle() {
				return
			}
			w.noteEventLoss()
			continue
		}
		if n == 0 {
			// Buffer overflow: Windows dropped events and tells us only "things
			// changed". Without a rescue, a lost final-write event strands a
			// dirty file forever.
			w.noteEventLoss()
			continue
		}
		w.parse(buf[:n])
	}
}

// reopenHandle replaces the watch handle after a ReadDirectoryChanges failure.
// Keeps trying while the watcher lives — the root may be briefly unopenable.
func (w *Watcher) reopenHandle() bool {
	pathW, err := windows.UTF16PtrFromString(w.root)
	if err != nil {
		return false
	}
	for w.ctx.Err() == nil {
		h, err := windows.CreateFile(pathW,
			windows.FILE_LIST_DIRECTORY,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			nil, windows.OPEN_EXISTING,
			windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
		if err == nil {
			w.mu.Lock()
			old := w.handle
			w.handle = h
			w.mu.Unlock()
			windows.CloseHandle(old)
			return true
		}
		if serr := sleepCtxVfs(w.ctx, 15*time.Second); serr != nil {
			return false
		}
	}
	return false
}

func sleepCtxVfs(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// noteEventLoss requests a skip-free reconcile pass: events were lost, so the
// ETag subtree skip's assumption (local state is good where the server is
// unchanged) no longer holds for local-side changes.
func (w *Watcher) noteEventLoss() {
	w.lostEvents.Store(true)
	w.Poke()
}

// parse walks the FILE_NOTIFY_INFORMATION records in b and dispatches them. A
// rename arrives as consecutive RENAMED_OLD then RENAMED_NEW records.
func (w *Watcher) parse(b []byte) {
	var renameOld string
	removed := map[string]string{}
	for off := 0; off+12 <= len(b); {
		next := *(*uint32)(unsafe.Pointer(&b[off]))
		action := *(*uint32)(unsafe.Pointer(&b[off+4]))
		nameLen := *(*uint32)(unsafe.Pointer(&b[off+8])) // bytes
		nameStart := off + 12
		if nameStart+int(nameLen) > len(b) {
			break
		}
		name := windows.UTF16ToString(unsafe.Slice((*uint16)(unsafe.Pointer(&b[nameStart])), nameLen/2))
		if skipName(name) {
			if next == 0 {
				break
			}
			off += int(next)
			continue
		}
		path := filepath.Join(w.root, name)
		switch action {
		case fileActionAdded, fileActionModified:
			// Only ADDED gets the move pairing: MODIFIED has no REMOVED
			// partner to pair WITH. A cross-directory move nonetheless
			// delivers BOTH for the destination — measured live 2026-09-15,
			// `cmd /c move` of an online-only stub produced REMOVED <src>,
			// ADDED <dst>, MODIFIED <dst-parent>, MODIFIED <dst> — because
			// clearing the placeholder's in-sync bit is itself an attribute
			// change. (A same-directory rename does not: RENAMED_OLD/NEW plus
			// a MODIFIED for the parent folder only.) So the ADDED is paired
			// into a MOVE and the MODIFIED still lands in the upload debounce
			// 800ms later, asking the write-back path to judge a file that was
			// only moved. handleChange is where that is answered.
			if action == fileActionAdded {
				if old, ok := w.moveSourceFor(path, removed); ok {
					w.NotifyRenamed(old, path)
					break
				}
			}
			w.cancelDelete(path)
			w.scheduleUpload(path)
		case fileActionRenamedOld:
			renameOld = path
		case fileActionRenamedNew:
			if renameOld != "" {
				w.cancelDelete(renameOld)
				old := renameOld
				renameOld = ""
				if w.beginRename(old, path) {
					go w.handleRename(old, path)
				} else {
					// The callback (NotifyRenamed) already claimed this pair.
					// A RENAMED pair proves no REMOVED is coming for old, so
					// any source key it recorded must not linger.
					w.forgetMoved(old)
					// Deduplicated, never dropped: check afterwards that the
					// item really did end up where the claimant's MOVE was
					// meant to put it. A redo inside the claim window looks
					// exactly like a duplicate report and is not one.
					go w.verifyPlacement(path)
				}
			} else {
				w.cancelDelete(path)
				w.scheduleUpload(path)
			}
		case fileActionRemoved:
			removed[strings.ToLower(filepath.Base(path))] = path
			w.scheduleDelete(path)
		}
		if next == 0 {
			break
		}
		off += int(next)
	}
}

// moveSourceFor asks whether an ADDED path is really the destination of a
// move: a placeholder whose identity names a path that was REMOVED — in this
// batch, or with a server delete still pending from an earlier one. The
// identity is what the server calls the file, so the match is exact and a new
// file that merely shares a name (a plain file, or a different identity) never
// qualifies.
func (w *Watcher) moveSourceFor(newPath string, removed map[string]string) (string, bool) {
	base := strings.ToLower(filepath.Base(newPath))
	var candidates []string
	if old, ok := removed[base]; ok && !strings.EqualFold(old, newPath) {
		candidates = append(candidates, old)
	}
	w.mu.Lock()
	for p := range w.delete {
		if strings.ToLower(filepath.Base(p)) == base && !strings.EqualFold(p, newPath) {
			candidates = append(candidates, p)
		}
	}
	w.mu.Unlock()
	if len(candidates) == 0 {
		return "", false
	}
	isDir := false
	if fi, lerr := os.Lstat(newPath); lerr == nil {
		isDir = fi.IsDir()
	}
	id, err := cfPlaceholderIdentity(newPath)
	if err != nil || len(id) == 0 {
		if isDir && errors.Is(err, cfapi.ErrNotPlaceholder) {
			// A plain directory has no identity of its own — a folder the
			// user created locally ("New folder") and then filled with server
			// folders is exactly what the filter's rename notice does not
			// cover (measured: only placeholders are reported). Its children
			// prove the move.
			return w.moveSourceFromChildren(newPath, candidates)
		}
		return "", false // plain file, or unreadable: not a move we can prove
	}
	for _, old := range candidates {
		if strings.EqualFold(string(id), w.serverFor(old, isDir)) {
			return old, true
		}
	}
	return "", false
}

// The probe below runs on the event-pump goroutine and opens a file per
// entry, so it is bounded twice over: it descends at most moveProbeDepth
// levels and inspects at most moveProbeEntries items before giving up. One
// placeholder anywhere in that window is all the proof it needs.
const (
	moveProbeDepth   = 3
	moveProbeEntries = 200
)

// moveSourceFromChildren proves a plain directory's move by what it holds: a
// placeholder somewhere beneath it whose identity lies under a candidate's
// server path was moved along with the directory. The search goes deeper than
// the immediate children because the user's own folders nest — "New folder\
// Sub" with the server folders dragged into Sub is plain all the way down to
// them, and reading that as a deletion took the whole subtree off the server.
func (w *Watcher) moveSourceFromChildren(newDir string, candidates []string) (string, bool) {
	prefixes := make([]string, len(candidates))
	for i, old := range candidates {
		prefixes[i] = w.serverFor(old, true) + "/"
	}
	found := ""
	seen := 0
	_ = filepath.WalkDir(newDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == newDir {
			return nil // an unreadable entry proves nothing; keep looking
		}
		rel, rerr := filepath.Rel(newDir, p)
		if rerr != nil {
			return nil
		}
		depth := strings.Count(rel, string(os.PathSeparator)) + 1
		if seen++; seen > moveProbeEntries {
			return fs.SkipAll
		}
		if id, ierr := cfPlaceholderIdentity(p); ierr == nil && len(id) > 0 {
			for i, prefix := range prefixes {
				if len(id) > len(prefix) && strings.EqualFold(string(id[:len(prefix)]), prefix) {
					found = candidates[i]
					return fs.SkipAll
				}
			}
		}
		if d.IsDir() && depth >= moveProbeDepth {
			return fs.SkipDir // at the cap: what it holds is out of reach
		}
		return nil
	})
	if found == "" {
		return "", false
	}
	return found, true
}

// scheduleUpload debounces handling of a path (editors fire many writes/save).
// heldRetry is how long to wait before re-checking a file somebody else has
// locked. Long enough not to hammer the server for the length of their editing
// session, short enough that the upload follows soon after they close it.
const heldRetry = 60 * time.Second

// moveWaitRetry is how long a change waits before looking again while a MOVE
// is in flight on its path or on a directory above it (see handleChange). A
// folder MOVE on Nextcloud takes seconds — the filecache is rewritten per
// descendant — so the debounce would poll it dozens of times; this keeps a
// multi-second move to a handful of checks while still sending the edit
// promptly once the move, and the baseline carry with it, has landed.
const moveWaitRetry = 2 * time.Second

// Failed-upload retry backoff (vars so tests can shrink them): 30s doubling to
// a 30-minute ceiling, forever — a transient outage heals fast, a persistent
// failure keeps announcing itself without hammering anything. Issue #1: before
// this there was NO retry — one failure and the file was ignored until the
// user happened to touch it again.
var (
	retryBase = 30 * time.Second
	retryMax  = 30 * time.Minute
)

// retryDelay returns the wait before retry n (1-based).
func retryDelay(n int) time.Duration {
	d := retryBase
	for i := 1; i < n; i++ {
		d *= 2
		if d >= retryMax {
			return retryMax
		}
	}
	return d
}

// scheduleUploadAfter is scheduleUpload with an explicit delay, for retrying a
// held upload without the tight debounce.
func (w *Watcher) scheduleUploadAfter(path string, d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if t, ok := w.upload[path]; ok {
		t.Reset(d)
		return
	}
	w.upload[path] = time.AfterFunc(d, func() {
		w.mu.Lock()
		delete(w.upload, path)
		w.mu.Unlock()
		w.handleChange(path)
	})
}

func (w *Watcher) scheduleUpload(path string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if t, ok := w.upload[path]; ok {
		t.Reset(uploadDebounce)
		return
	}
	w.upload[path] = time.AfterFunc(uploadDebounce, func() {
		w.mu.Lock()
		delete(w.upload, path)
		w.mu.Unlock()
		w.handleChange(path)
	})
}

// scheduleUploadIfIdle arms an upload for a path only when nothing is pending
// or running for it — reconcile's rescue of stuck-dirty files must neither
// reset a live backoff timer nor double up on an in-flight upload.
func (w *Watcher) scheduleUploadIfIdle(path string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.upload[path]; ok {
		return
	}
	if _, busy := w.inflight[strings.ToLower(path)]; busy {
		return
	}
	w.upload[path] = time.AfterFunc(uploadDebounce, func() {
		w.mu.Lock()
		delete(w.upload, path)
		w.mu.Unlock()
		w.handleChange(path)
	})
}

// cancelInflight aborts a running upload for path, if any: a delete or rename
// event supersedes it, and letting the upload finish would recreate the file
// on the server after the DELETE (then locally, via reconcile).
func (w *Watcher) cancelInflight(path string) {
	w.mu.Lock()
	cancel := w.inflight[strings.ToLower(path)]
	if t, ok := w.upload[path]; ok { // a queued upload for the path is equally moot
		t.Stop()
		delete(w.upload, path)
	}
	// The FORCED flag goes with them. It can sit behind a held or backed-off
	// re-arm for up to half an hour, and this path is being vacated (a rename
	// or a delete): left behind, the next occupant of the name would run
	// forced — past the in-sync gate AND past the identity guard — which for
	// a stub moved in is a full hydrate and PUT of a file the server already
	// has, racing that occupant's own move.
	delete(w.forced, strings.ToLower(path))
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// scheduleDelete debounces a server-side delete so a transient remove (rename
// source, atomic save) can be cancelled before it fires.
func (w *Watcher) scheduleDelete(path string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if t, ok := w.delete[path]; ok {
		t.Reset(deleteDebounce)
		return
	}
	w.delete[path] = time.AfterFunc(deleteDebounce, func() {
		// Pending hands over to running under one lock, so a folder waiting
		// on this path never sees it in neither state. handleDelete counts
		// its own run; this mark covers only the gap and is released after.
		w.mu.Lock()
		delete(w.delete, path)
		w.markDeletingLocked(strings.ToLower(path), true)
		w.mu.Unlock()
		w.handleDelete(path)
		w.markDeleting(strings.ToLower(path), false)
	})
}

// scheduleDeleteAfter re-arms a failed server delete with an explicit delay.
// It rides the same timer map as the debounce, so a path that reappears before
// the retry fires still cancels it (the resurrection check in handleDelete
// backs that up).
func (w *Watcher) scheduleDeleteAfter(path string, d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if t, ok := w.delete[path]; ok {
		t.Reset(d)
		return
	}
	w.delete[path] = time.AfterFunc(d, func() {
		// Pending hands over to running under one lock, so a folder waiting
		// on this path never sees it in neither state. handleDelete counts
		// its own run; this mark covers only the gap and is released after.
		w.mu.Lock()
		delete(w.delete, path)
		w.markDeletingLocked(strings.ToLower(path), true)
		w.mu.Unlock()
		w.handleDelete(path)
		w.markDeleting(strings.ToLower(path), false)
	})
}

// cancelDelete stops path's pending delete timer, if any, reporting whether
// one was actually pending.
func (w *Watcher) cancelDelete(path string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if t, ok := w.delete[path]; ok {
		t.Stop()
		delete(w.delete, path)
		return true
	}
	return false
}

// NotifyRenamed records a rename or move the Cloud Files filter reported
// (cfapi.SetRenameHandler) and pushes it to the server as a MOVE. Windows
// reports a move between directories to ReadDirectoryChangesW only as
// REMOVED + ADDED, which the watcher used to turn into a server DELETE of the
// source — and, for an online-only item, an upload of the destination that
// could never succeed (its data lived at the path just deleted). If the
// REMOVED for this move already arrived (a delete timer is pending for
// oldPath), cancelling it is enough; otherwise the REMOVED is still to come,
// so the source is remembered just long enough for that one event. Issue #7.
// Safe to call from any goroutine; returns immediately.
// The delete bookkeeping runs even when the claim is lost, and only the PUSH
// is gated: every report of a move brings its own REMOVED (or a pending timer
// for one), and skipping the bookkeeping on a repeat left that REMOVED to fire
// as a server DELETE of the file.
func (w *Watcher) NotifyRenamed(oldPath, newPath string) {
	if w.ctx.Err() != nil {
		return
	}
	claimed := w.beginRename(oldPath, newPath)
	if w.cancelDelete(oldPath) {
		// The move's REMOVED already arrived and its delete is now cancelled —
		// settled. Any source key an earlier report of this same move recorded
		// has nothing left to consume it, so it must not linger and swallow a
		// later, genuine delete at the vacated path.
		w.forgetMoved(oldPath)
	} else {
		// No delete timer was pending — the move's REMOVED hasn't reached us
		// yet. Remember the source so that REMOVED, when it arrives, isn't
		// mistaken for a genuine delete (recentlyMoved consumes this key).
		w.mu.Lock()
		w.moved[strings.ToLower(oldPath)] = time.Now()
		w.mu.Unlock()
	}
	if !claimed {
		// The RENAMED pair (or an earlier report) already pushed it — or this
		// is a redo of the same move inside the claim window, which nobody
		// has pushed. Both look identical from here, so the placement is
		// checked once the claimant's MOVE (if there is one) has settled.
		go w.verifyPlacement(newPath)
		return
	}
	go w.handleRename(oldPath, newPath)
}

// movedKeyTTL returns how long a key in w.moved stays valid. A PAIR key only
// has to outlive the gap between one move's two reporters; a SOURCE key has
// to outlive the delete debounce that follows it.
func movedKeyTTL(key string) time.Duration {
	if strings.Contains(key, "\x00") {
		return renameClaimTTL
	}
	return movedSourceTTL
}

// beginRename claims a rename for processing, recording only the pair key,
// and reports whether this caller is the first to claim it — the same
// rename can arrive both as a RENAMED pair and as the filter's callback, and
// must be pushed once. NotifyRenamed separately records a source key, and
// only when one is actually needed. Expired keys are pruned here: between
// delete events (which is where recentlyMoved prunes) nothing else would ever
// clear a pair key, and a busy mount renames constantly.
func (w *Watcher) beginRename(oldPath, newPath string) bool {
	key := strings.ToLower(oldPath) + "\x00" + strings.ToLower(newPath)
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	for k, t := range w.moved {
		if now.Sub(t) > movedKeyTTL(k) {
			delete(w.moved, k)
		}
	}
	if t, ok := w.moved[key]; ok && now.Sub(t) <= renameClaimTTL {
		return false
	}
	w.moved[key] = now
	return true
}

// forgetMoved removes path's recorded move-source key, if any. Used when a
// RENAMED pair proves no REMOVED is coming for a source the callback already
// claimed (NotifyRenamed ran first) — the key must not linger and wrongly
// suppress a later, unrelated, genuine delete at that path.
func (w *Watcher) forgetMoved(path string) {
	w.mu.Lock()
	delete(w.moved, strings.ToLower(path))
	w.mu.Unlock()
}

// verifyPlacement answers a rename report the pair claim deduplicated, a
// moment after the fact: is the item at newPath really where the server keeps
// it? Meant to be started as `go w.verifyPlacement(newPath)`.
//
// A deduplicated report is usually a genuine duplicate — the RENAMED pair and
// the filter's rename-completion callback describing ONE move — and the
// claimant's MOVE puts the file right, so this finds nothing to do. It is not
// always: a user who undoes a move and redoes it inside renameClaimTTL (Ctrl+Z
// then Ctrl+Y, or a drag back and forth) makes the same (old,new) pair a
// second time, and that redo is a real move nobody pushes. Dropping it left
// the file at newPath on this PC and at the old path everywhere else,
// indefinitely — measured on the VM 2026-09-15 (T2b). Reconcile cannot mop
// that up either: a deduplicated move changes nothing on the server, so the
// ETag subtree skip keeps its identity check from ever looking again until a
// full sweep (the first pass after a start, or a lost-events pass).
//
// It decides nothing that check would not decide: the placeholder's own
// identity is the evidence, and moveServer owns the retries, the 404 fallback
// and finishRename.
func (w *Watcher) verifyPlacement(newPath string) {
	if w.ctx.Err() != nil {
		return
	}
	// The two gates handleRename opens with: a rename we applied during
	// down-sync is the server's own doing, and the sync root never moves.
	rel := w.remoteFor(newPath)
	if rel == "" || w.isSuppressed(newPath) {
		return
	}
	if !w.beginVerify(newPath) {
		return // a check for this path is already pending; one answers them all
	}
	defer w.endVerify(newPath)
	if sleepCtxVfs(w.ctx, renameVerifyDelay) != nil {
		return
	}
	// A MOVE onto this path, or onto any folder above it, means somebody is
	// already doing the job — wait it out.
	// The mark covers its whole retry chain, so it is never a momentary gap we
	// could usefully fill; past renameVerifyMax that chain is either still
	// retrying or has given up and reported, and a MOVE from here would only
	// race it (the loser 404s).
	deadline := time.Now().Add(renameVerifyMax)
	for w.moveInFlightOrAbove(newPath) {
		if time.Now().After(deadline) {
			w.ops.Log("vfs placement of %s unverified: a move is still in flight", rel)
			return
		}
		if sleepCtxVfs(w.ctx, renameVerifyPoll) != nil {
			return
		}
	}
	if w.isSuppressed(newPath) {
		// Checked again after the wait, not only at entry: a detach parking a
		// vanished share (Deck #557) can claim this path while we sleep, and
		// what follows would push a MOVE for work that is ours, not the
		// user's.
		return
	}
	fi, err := os.Lstat(newPath)
	if err != nil {
		return // gone: whatever happened to it next brings its own event
	}
	id, err := cfPlaceholderIdentity(newPath)
	if err != nil || len(id) == 0 {
		return // a plain item, or unreadable: no identity to judge placement by
	}
	if strings.EqualFold(string(id), w.serverFor(newPath, fi.IsDir())) {
		// The overwhelmingly common case: the claimant's MOVE placed it.
		// Deliberately silent — this runs on every dual-reported rename and
		// Ops.Log is the user-visible log.
		return
	}
	if w.moveInFlightOrAbove(newPath) {
		return // one started in the gap; it reads the same evidence
	}
	w.moveServer(string(id), newPath, fi.IsDir())
}

// beginVerify claims the deferred placement check for path, reporting whether
// this caller got it. endVerify releases it when the check is done — including
// the MOVE it may have issued, so a report arriving mid-MOVE is a no-op too.
func (w *Watcher) beginVerify(path string) bool {
	key := strings.ToLower(path)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.verifying[key] {
		return false
	}
	w.verifying[key] = true
	return true
}

func (w *Watcher) endVerify(path string) {
	w.mu.Lock()
	delete(w.verifying, strings.ToLower(path))
	w.mu.Unlock()
}

// recentlyMoved reports whether path (or a descendant of a moved directory)
// is the source of a move claimed within movedSourceTTL. An exact match is
// consumed here — it exists only for the one REMOVED it was recorded for, so
// a later, unrelated delete at the same path is never also suppressed; a
// directory-ancestor match is left in place since more than one REMOVED may
// still follow it. TTL pruning is only a backstop against a REMOVED that
// never comes.
func (w *Watcher) recentlyMoved(path string) bool {
	lp := strings.ToLower(path)
	w.mu.Lock()
	defer w.mu.Unlock()
	for p, t := range w.moved {
		if time.Since(t) > movedKeyTTL(p) {
			delete(w.moved, p)
			continue
		}
		if strings.Contains(p, "\x00") {
			continue // a pair key, not a source path
		}
		if lp == p {
			delete(w.moved, p) // consumed by its own REMOVED — one shot
			return true
		}
		if strings.HasPrefix(lp, p+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// skipName reports whether a change to rel (a sync-root-relative path) should be
// ignored: the diff engine's download temp files and well-known OS/editor
// temporaries must never be pushed to the server.
func skipName(rel string) bool {
	base := rel
	if i := strings.LastIndexAny(rel, `\/`); i >= 0 {
		base = rel[i+1:]
	}
	// Windows filenames are case-insensitive; compare accordingly (a live
	// "Desktop.ini" once slipped past an exact match and was conflict-copied
	// to the server).
	lb := strings.ToLower(base)
	switch {
	case strings.HasSuffix(lb, ".nimbo-part"): // engine download temp
		return true
	case lb == "thumbs.db" || lb == "desktop.ini" || lb == ".ds_store":
		return true
	case strings.HasPrefix(base, "~$") || strings.HasPrefix(lb, ".~lock."):
		return true
	case strings.HasSuffix(lb, ".tmp") || strings.HasSuffix(lb, ".~tmp") || strings.HasSuffix(lb, ".crdownload"):
		return true
	// The official Nextcloud/ownCloud client keeps its sync database and log
	// INSIDE the sync folder; they survive a migration as pure pollution.
	case lb == ".nextcloudsync.log" || lb == ".owncloudsync.log":
		return true
	case strings.HasPrefix(lb, ".sync_") &&
		(strings.HasSuffix(lb, ".db") || strings.HasSuffix(lb, ".db-wal") ||
			strings.HasSuffix(lb, ".db-shm") || strings.HasSuffix(lb, ".db-journal")):
		return true
	}
	return false
}

// report surfaces a completed op (no-op if no Report callback is set).
func (w *Watcher) report(kind, remotePath string, err error) {
	if w.ops.Report != nil {
		w.ops.Report(kind, remotePath, err)
	}
}

// noteMoveWait records that path is waiting on a move and reports whether
// this is the first wait of the current stretch (the one worth a log line).
// clearMoveWait ends the stretch once the change goes through, so a later,
// unrelated move of the same path is announced again.
func (w *Watcher) noteMoveWait(path string) bool {
	key := strings.ToLower(path)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.moveWaitNoted == nil {
		w.moveWaitNoted = map[string]bool{}
	}
	if w.moveWaitNoted[key] {
		return false
	}
	w.moveWaitNoted[key] = true
	return true
}

func (w *Watcher) clearMoveWait(path string) {
	w.mu.Lock()
	delete(w.moveWaitNoted, strings.ToLower(path))
	w.mu.Unlock()
}

// noteCorrupt reports, once per path, an entry Windows refuses to open with
// ERROR_CLOUD_FILE_METADATA_CORRUPT, and says whether the error WAS that fault
// — the caller has nothing left to try on such an entry and should stop
// treating it as something it can act on.
//
// Such an entry is a Windows Cloud Files fault (one long placeholder identity
// in a batch corrupts its batch-mates — see cfapi.longIdentityBytes): it
// cannot be opened, repaired or deleted by any normal means, reconcile skips
// it on every pass, and without this the user sees "Up to date" over a file
// they cannot open. Other errors are the caller's business and are ignored
// here.
func (w *Watcher) noteCorrupt(full string, err error) bool {
	if !errors.Is(err, windows.ERROR_CLOUD_FILE_METADATA_CORRUPT) {
		return false
	}
	key := strings.ToLower(full)
	w.mu.Lock()
	if w.corruptReported == nil {
		w.corruptReported = map[string]bool{}
	}
	seen := w.corruptReported[key]
	w.corruptReported[key] = true
	w.mu.Unlock()
	if seen {
		return true
	}
	// The sync ROOT has no remote path of its own, and an empty one renders as
	// "." everywhere it is shown. Name it the way the user sees it.
	name := w.remoteFor(full)
	if name == "" {
		name = filepath.Base(full)
	}
	w.ops.Log("vfs %s has corrupt cloud-file metadata (a Windows Cloud Files fault); the steps to clear it are in the activity feed", name)
	w.report("corrupt", name, errors.New(corruptRecovery(full)))
	return true
}

// corruptRecovery is the whole recovery for one broken entry, as the activity
// feed shows it.
//
// It is deliberately self-contained. The detach-and-delete recipe is not
// guessable, the entry cannot be cleared any other way, and the only document
// that carries the recipe is origin-only — excluded from every public source
// snapshot — so a "see Troubleshooting" sent the one user who ever saw this to
// a page that does not exist for them.
// The commands are meant to be pasted, so both paths are SINGLE-quoted with
// any apostrophe in them doubled — PowerShell's own escape. Double quotes
// would expand $ and backticks, and apostrophes in folder names are common.
// No product name appears: white-label builds run this code too.
func corruptRecovery(full string) string {
	drive := filepath.VolumeName(full)
	if drive == "" {
		drive = "C:"
	}
	q := strings.ReplaceAll(full, "'", "''")
	return fmt.Sprintf(`Windows reports the cloud-file metadata of %s as corrupt `+
		`(a Windows Cloud Files fault); it cannot be opened, repaired or deleted `+
		`normally and the server copy is unaffected. To clear it: quit this app, then in `+
		`an administrator PowerShell run: fltmc detach cldflt %s ; `+
		`fsutil reparsepoint delete '%s' ; `+
		`Remove-Item -LiteralPath '\\?\%s' -Recurse -Force ; `+
		`fltmc attach cldflt %s (this briefly pauses every cloud provider on that `+
		`drive, including OneDrive). This app re-creates the entry properly on its next pass.`,
		filepath.Base(full), drive, q, q, drive)
}

// recordBaseline notes the server ETag a placeholder now mirrors (no-op if unset).
func (w *Watcher) recordBaseline(remotePath, etag string) {
	if w.ops.RecordBaseline != nil && etag != "" {
		w.ops.RecordBaseline(remotePath, etag)
	}
}

// baselineFor returns the recorded ETag for a remote path (used for both file
// change detection and the directory-subtree skip).
func (w *Watcher) baselineFor(remotePath string) (string, bool) {
	if w.ops.Baseline == nil {
		return "", false
	}
	return w.ops.Baseline(remotePath)
}

// recordBaselines records a whole set of baselines in one write (no-op if
// empty). The batch hook is what the store wants — it persists its entire file
// per call — so per-item recording of a directory's pull cost one full rewrite
// per item. Without the hook the one-item path still works, unbatched.
func (w *Watcher) recordBaselines(m map[string]string) {
	if len(m) == 0 {
		return
	}
	if w.ops.RecordBaselines == nil {
		for remote, etag := range m {
			w.recordBaseline(remote, etag)
		}
		return
	}
	w.ops.RecordBaselines(m)
}

// moveBaselines carries a whole subtree's baselines across a MOVE in one write
// (see moveBaseline for why the ETag travels unchanged). Pairs that cannot
// move anything — either end empty, or the same path on both ends — are
// dropped here so the store is never handed busywork.
func (w *Watcher) moveBaselines(pairs [][2]string) {
	keep := make([][2]string, 0, len(pairs))
	for _, p := range pairs {
		if p[0] == "" || p[1] == "" || strings.EqualFold(p[0], p[1]) {
			continue
		}
		keep = append(keep, p)
	}
	if len(keep) == 0 {
		return
	}
	if w.ops.MoveBaselines == nil {
		for _, p := range keep {
			w.moveBaseline(p[0], p[1])
		}
		return
	}
	w.ops.MoveBaselines(keep)
}

// moveBaseline carries the conflict baseline of a server path across a MOVE
// the server has applied: the ETag Nextcloud reports for a file is unchanged
// by moving it (measured live 2026-09-15: PUT, MOVE, MOVE back — the same
// ETag each time), so the placeholder mirrors exactly the version it did
// before, now under the new name. Without this the kept upload after an
// edit-then-move saw no baseline at all and, for any real edit, parked the
// server's copy as a spurious "conflicted copy"; every plain move was also
// followed by a needless "refreshed (server changed)" of the new path.
func (w *Watcher) moveBaseline(srcRemote, dstRemote string) {
	if srcRemote == "" || dstRemote == "" || strings.EqualFold(srcRemote, dstRemote) {
		return
	}
	if base, ok := w.baselineFor(srcRemote); ok && base != "" {
		w.recordBaseline(dstRemote, base)
	}
	if w.ops.ForgetBaseline != nil {
		w.ops.ForgetBaseline(srcRemote)
	}
}

// recordFileID / fileIDFor / dropFileID persist the server oc:fileid per remote
// path (no-ops if the hooks are unset), so down-sync can recognise renames.
func (w *Watcher) recordFileID(remotePath, fileid string) {
	if w.ops.RecordFileID != nil && fileid != "" {
		w.ops.RecordFileID(remotePath, fileid)
	}
}

func (w *Watcher) fileIDFor(remotePath string) (string, bool) {
	if w.ops.FileID == nil {
		return "", false
	}
	return w.ops.FileID(remotePath)
}

func (w *Watcher) dropFileID(remotePath string) {
	if w.ops.DropFileID != nil {
		w.ops.DropFileID(remotePath)
	}
}

// pullRename applies a detected server-side rename: the in-sync placeholder
// oldFull (gone from the server) and a server addition y share an oc:fileid, so
// the server renamed it. Move the local placeholder to y (preserving any local
// hydration) and repoint its identity, instead of delete+recreate. Returns false
// — caller falls back to a plain delete — if the move can't be applied.
func (w *Watcher) pullRename(localDir, oldFull string, y cfapi.PlaceholderInfo, fileid string) bool {
	newFull := filepath.Join(localDir, y.Name)
	if _, err := os.Lstat(newFull); err == nil {
		return false // target already exists locally — don't clobber
	}
	oldRemote := w.remoteFor(oldFull)
	// Suppress BOTH paths BEFORE the move so the watcher's own RENAMED events
	// don't bounce this (already-applied-on-server) rename back as a move/upload.
	w.suppressDelete(oldFull)
	w.suppressDelete(newFull)
	if err := os.Rename(oldFull, newFull); err != nil {
		w.ops.Log("vfs pull-rename %s -> %s: %v (will delete instead)", oldRemote, string(y.Identity), err)
		return false
	}
	if err := cfUpdateIdentity(newFull, y.Identity); err != nil {
		// Repoint failed — make the moved file fetch correctly from its new path.
		w.ops.Log("vfs pull-rename repoint %s: %v (refreshing)", string(y.Identity), err)
		if rerr := cfRefreshPlaceholder(newFull, y.Identity, y.Size, y.ModTime); rerr != nil {
			w.ops.Log("vfs pull-rename refresh %s: %v", string(y.Identity), rerr)
		}
	}
	w.recordBaseline(string(y.Identity), y.ETag)
	w.recordFileID(string(y.Identity), fileid)
	w.dropFileID(oldRemote)
	w.ops.Log("vfs pulled rename %s -> %s", oldRemote, string(y.Identity))
	w.report("move", string(y.Identity), nil)
	return true
}

// serverChanged reports whether the remote copy r differs from the local file —
// a different size, or a meaningfully newer modified time. This is a heuristic:
// it can miss an edit that keeps the same size and (near-)same mtime. Prefer
// remoteChanged, which uses the ETag baseline when available.
func serverChanged(r cfapi.PlaceholderInfo, fi os.FileInfo) bool {
	if r.Size != fi.Size() {
		return true
	}
	return r.ModTime.After(fi.ModTime().Add(2 * time.Second))
}

// remoteChanged reports whether the server copy r differs from the in-sync local
// placeholder for remotePath. It prefers the ETag baseline (authoritative — any
// server-side content change alters the ETag, so this catches same-size edits the
// size/mtime heuristic would miss and leave silently stale), and falls back to
// serverChanged when no ETag or recorded baseline is available.
func (w *Watcher) remoteChanged(r cfapi.PlaceholderInfo, fi os.FileInfo, remotePath string) bool {
	if r.ETag != "" && w.ops.Baseline != nil {
		if base, ok := w.ops.Baseline(remotePath); ok && base != "" {
			return r.ETag != base
		}
	}
	return serverChanged(r, fi)
}

// remoteFor maps a local path to its files-root-relative remote path. Returns
// "" for the sync root itself (which must never be deleted/moved).
func (w *Watcher) remoteFor(path string) string {
	rel, err := filepath.Rel(w.root, path)
	if err != nil || rel == "." {
		return ""
	}
	return strings.Trim(w.remoteRoot+"/"+filepath.ToSlash(rel), "/")
}

// serverFor is remoteFor plus disguised-type escaping: the name the SERVER knows
// this path by. Everything server-bound must use it — the PUT/DELETE/MOVE target
// AND the placeholder identity, because the identity is what the OS hands back on
// hydration (fetchDataCallback -> DownloadRange), so a raw identity on a
// disguised file is a 404 on every open.
//
// isDir short-circuits: escaping covers file basenames only, and a directory
// that happens to match an opted-in name must keep its real name or its ETag key
// stops matching the parent listing and the reconcile subtree-skip dies.
//
// remoteFor (the LOCAL name) remains the right thing for anything user-visible.
func (w *Watcher) serverFor(path string, isDir bool) string {
	rel := w.remoteFor(path)
	if rel == "" || isDir || w.ops.Encode == nil {
		return rel
	}
	return w.ops.Encode(rel)
}

// localName maps a RAW server path back to its user-visible form, for the
// activity feed and error toasts — nobody should be shown ".htaccess.nimboesc".
func (w *Watcher) localName(remote string) string {
	if w.ops.Decode == nil {
		return remote
	}
	return w.ops.Decode(remote)
}

// forceChange arms path's Mkdir/Upload branch to run even though it is
// currently in-sync (NeedsUpload == false) — moveServer's 404 fallback for a
// destination that already has data (or is a directory), where a plain
// handleChange would see nothing to do and silently no-op. The upload itself
// runs on the debounce timer's own goroutine via scheduleUploadIfIdle, same
// as every other upload in this file — moveServer runs inside a reconcile
// pass (reconMu held), and a big forced upload must not block it. If a timer
// or an in-flight upload already exists for path, scheduleUploadIfIdle does
// nothing; the flag is picked up by whichever handleChange runs next for
// that path (its own re-arm on completion, if it's still needed then).
func (w *Watcher) forceChange(path string) {
	w.mu.Lock()
	w.forced[strings.ToLower(path)] = true
	w.mu.Unlock()
	w.scheduleUploadIfIdle(path)
}

// takeForced consumes and reports whether path was queued via forceChange.
// Called once, at the very top of handleChange, so a lingering flag is
// consumed by whichever run of handleChange for that path comes next —
// never by an unrelated later call.
func (w *Watcher) takeForced(path string) bool {
	key := strings.ToLower(path)
	w.mu.Lock()
	defer w.mu.Unlock()
	forced := w.forced[key]
	delete(w.forced, key)
	return forced
}

// reforce re-asserts the forced flag for a run that is about to be re-armed.
// takeForced consumes it at the top of handleChange, so every re-arm out of a
// FORCED run — held, busy, the failure backoff, changed-during-upload — would
// otherwise come back as an ordinary run, and an ordinary run of that file is
// exactly what the identity guard above refuses (its identity still names the
// dead source the 404 fallback was called for). The flag only: the re-arm
// keeps its own timing, so a held file still waits heldRetry and a failed one
// still walks the backoff.
func (w *Watcher) reforce(path string, forced bool) {
	if !forced {
		return
	}
	w.mu.Lock()
	w.forced[strings.ToLower(path)] = true
	w.mu.Unlock()
}

// handleChange uploads one changed path if it's a user create/modify, then marks
// it in-sync so the next notification (from our own write) is ignored.
//
// One upload per path at a time: a change event landing mid-upload (they take
// hours on huge files) queues exactly one more pass instead of starting a
// concurrent duplicate. Failures re-arm themselves with backoff — the file
// keeps retrying until it lands or changes, never silently ignored (issue #1).
func (w *Watcher) handleChange(path string) {
	if w.ctx.Err() != nil {
		return
	}
	// Consumed here, up front, so it's this specific run that acts on it —
	// never a later, unrelated call for the same path. If this run turns out
	// to be busy (below) and re-arms via `again`, the re-armed run finds the
	// item already in-sync (the in-flight upload's MarkInSync got there
	// first) and correctly no-ops; the forced upload it was meant to cover
	// already happened.
	forced := w.takeForced(path)
	key := strings.ToLower(path)
	uctx, ucancel := context.WithCancel(w.ctx)
	defer ucancel()
	w.mu.Lock()
	if _, busy := w.inflight[key]; busy {
		w.again[key] = true
		w.mu.Unlock()
		return
	}
	w.inflight[key] = ucancel
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		delete(w.inflight, key)
		rearm := w.again[key]
		delete(w.again, key)
		w.mu.Unlock()
		if rearm {
			w.scheduleUpload(path)
		}
	}()
	if w.isSuppressed(path) {
		return // a change we made during down-sync (e.g. a pulled rename) — not the user's
	}
	info, err := os.Lstat(path)
	if err != nil {
		return // vanished (temp file) — deletions handled separately
	}
	ch, err := cfInspect(path)
	if err != nil {
		w.ops.Log("vfs inspect %s: %v", path, err)
		return
	}
	// A MOVE is running on this path or on a directory above it: nothing here
	// can be decided yet, so wait — the same argument moveInFlightOrAbove
	// already makes for the placement check.
	//
	// The sharp edge is a descendant of a folder being moved. finishRename's
	// walk repoints every descendant and then carries the whole subtree's
	// baselines in ONE write at the end, so for the length of that walk a
	// descendant's identity already names the NEW path — which is exactly
	// what the foreign-identity refusal below tests, so it has nothing to say
	// — while its baseline is still recorded under the OLD one. Uploading in
	// that window makes the conflict check read "no baseline recorded" over a
	// server copy the MOVE has just placed, and it parks the server's own
	// version as a spurious "conflicted copy": the symptom the carry exists
	// to prevent, on the operation it exists for. The moved item itself is
	// covered by the same check.
	//
	// Re-arm rather than drop, at a plain short delay and without touching
	// any attempt count — a move is not a failure, and nothing else is
	// guaranteed to come back for a file whose event we swallow. Each wait
	// costs one Lstat and one attributes-only inspect, no network.
	if w.moveInFlightOrAbove(path) {
		// Said once per wait, not once per 2 s poll: a big folder move on a
		// slow server would otherwise put a line per file per poll into the
		// log users send us.
		if w.noteMoveWait(path) {
			w.ops.Log("vfs %s: a move is in flight on it or above it — waiting before acting on this change", w.remoteFor(path))
		}
		w.reforce(path, forced)
		w.scheduleUploadAfter(path, moveWaitRetry)
		return
	}
	w.clearMoveWait(path)
	// A placeholder whose identity still names ANOTHER server path is a move
	// the server has not been told about (in flight, retrying, deduplicated by
	// the pair claim — or owed from a MOVE chain that gave up, or a 404
	// fallback that repointed nothing before wave 5). Whatever it holds must
	// reach the server at the path the MOVE creates, after the repoint:
	// uploading it here lands a second file, and MarkInSync would leave the
	// identity dead (it only ever sets the bit on an existing placeholder, so
	// the copy would claim to be in sync at a server path it does not occupy
	// — the file at both paths, permanently). The heal is skipped with it:
	// the arrows stay, truthfully, until the MOVE lands.
	//
	// Refusing is not enough on its own. Nothing else is guaranteed to come
	// back for this file — reconcile's foreign-identity branch only runs on a
	// full sweep, and for a file the server DOES hold at this name it never
	// runs at all — so a placeholder whose identity is simply DEAD would be
	// refused on every event and every pass, for good. So hand it to the
	// placement check, which is the one piece that can settle it either way:
	// a live or retrying move repoints it (and the check no-ops), a dead
	// source 404s into the fallback that uploads this copy at its own name and
	// repoints it. Deduplicated per path, so an event storm costs one check.
	// forced is the one caller (moveServer's 404 fallback) for which uploading
	// at the new name IS the plan.
	if !forced && !ch.IsDir && ch.Placeholder && !ch.InSync {
		if id, ierr := cfPlaceholderIdentity(path); ierr == nil && len(id) > 0 &&
			!strings.EqualFold(string(id), w.serverFor(path, false)) {
			w.ops.Log("vfs %s still names %s: a move owns it; checking where it belongs", w.remoteFor(path), string(id))
			go w.verifyPlacement(path)
			return
		}
	}
	// An ONLINE-ONLY stub holds no local content the server has not got, so a
	// write-back verdict on one can only ever be the rename artefact: a
	// cross-directory move clears the placeholder's in-sync bit AND delivers
	// FILE_ACTION_MODIFIED for the destination (both measured live
	// 2026-09-15), which lands here 800ms later as a change to handle. On a
	// slow server that debounce beats the MOVE's own repoint, and uploading a
	// stub means hydrating it just to send the bytes back — which is how the
	// VM ended up with the same file at BOTH paths, the second one parked as a
	// conflicted copy. The move path and reconcile's identity check own this
	// item; the only thing to do here is the not-dirty housekeeping below.
	// (forced still wins: that flag exists for the 404-fallback upload, where
	// the stub really is the only copy of something.)
	online := !ch.IsDir && cfIsDehydrated(info, filepath.ToSlash(path))
	if (!ch.NeedsUpload || online) && !forced {
		// The filter clears the in-sync bit on every rename or move, so a
		// clean FILE that merely moved is left wearing Explorer's
		// "sync pending" arrows — on itself and on every folder above it —
		// with nothing that will ever look at it again. finishRename's
		// MARK_IN_SYNC repoint normally restores it, but not when the move was
		// deduped as a repeat inside the claim window (no finishRename runs at
		// all). Put the bit back; a later repoint on top is harmless. Never on
		// a file with a pending upload: that bit IS the pending upload.
		//
		// DIRECTORIES are excluded, and that exclusion is load-bearing, not
		// tidiness: our directory placeholders are deliberately left NOT
		// in-sync because that is what makes the shell ask them to populate,
		// and a directory marked in-sync enumerates EMPTY forever
		// (cfapi.TestInSyncDirStillPopulates). Folders get FILE_ACTION_MODIFIED
		// constantly — a move into one delivers MODIFIED for the destination
		// FOLDER as well as the file — so this branch sees them often.
		// Marking a populated directory settled is SweepDirsInSync's job,
		// which knows to wait for the PARTIAL bit to clear.
		if !ch.IsDir && ch.Placeholder && !ch.InSync && !ch.NeedsUpload {
			if serr := cfSetInSync(path); serr != nil {
				w.ops.Log("vfs restore in-sync %s: %v", path, serr)
			} else {
				w.ops.Log("vfs restored in-sync state for %s (moved, not modified)", w.remoteFor(path))
			}
		}
		// Nothing to upload — but the event may be a PIN change: Explorer's
		// "Free up space" sets UNPINNED and waits for US to dehydrate; until
		// then the item and its ancestors wear sync-pending arrows.
		if ok, serr := cfSettlePin(path); serr != nil {
			w.ops.Log("vfs settle pin %s: %v", path, serr)
		} else if ok {
			w.ops.Log("vfs freed up space for %s", w.remoteFor(path))
		}
		if cfPinnedDehydrated(path) {
			w.requestHydration(path)
		}
		return
	}
	remote := w.remoteFor(path)
	if remote == "" {
		return
	}
	// server = what the server calls it (escaped for a disguised file); remote =
	// what the user calls it. The two differ only for disguised types, and mixing
	// them up is what #550 was.
	server := w.serverFor(path, ch.IsDir)
	if ch.IsDir {
		if err := w.ops.Mkdir(w.ctx, server); err != nil {
			w.ops.Log("vfs mkdir %s: %v", server, err)
			w.report("mkdir-remote", remote, err)
			// Forced like every other re-arm out of a forced run: an ordinary
			// retry of a directory placeholder has nothing to upload and
			// no-ops at the gate, so the folder the 404 fallback asked for
			// would wait for the next full sweep while uploads into it 409.
			w.reforce(path, forced)
			w.retryLater(path, key)
			return
		}
		w.report("mkdir-remote", remote, nil)
	} else {
		if err := w.ops.Upload(uctx, path, server); err != nil {
			switch {
			case errors.Is(err, ErrHeldByLock):
				// Somebody else has the file locked. Re-arm rather than report:
				// a hold is not a failure. MarkInSync is deliberately skipped too
				// — marking it synced would let a later refresh dehydrate the
				// edit away.
				w.ops.Log("vfs upload %s held: someone else has it locked", server)
				w.reforce(path, forced)
				w.scheduleUploadAfter(path, heldRetry)
			case isFileBusy(err):
				// The file is still being written locally (Explorer mid-copy, an
				// app mid-save). Same treatment: quietly try again once the
				// writer has had time to finish — but a file busy for a long
				// time deserves a visible entry, not eternal silence.
				w.ops.Log("vfs upload %s busy locally: %v", server, err)
				w.mu.Lock()
				w.busyCount[key]++
				n := w.busyCount[key]
				w.mu.Unlock()
				if n%30 == 0 { // ~every 30 minutes at the 60s retry cadence
					w.report("upload", remote, fmt.Errorf("the file has been in use by another program for a while — this app keeps retrying"))
				}
				w.reforce(path, forced)
				w.scheduleUploadAfter(path, heldRetry)
			case w.ctx.Err() != nil:
				// Shutting down — the reconcile rescue re-arms it next start.
			case uctx.Err() != nil:
				// Cancelled on purpose: a delete or rename superseded this
				// upload and owns the path now. Drop quietly.
				w.ops.Log("vfs upload %s cancelled (superseded)", server)
			default:
				w.ops.Log("vfs upload %s -> %s: %v", path, server, err)
				w.report("upload", remote, err)
				w.reforce(path, forced)
				w.retryLater(path, key)
			}
			return
		}
		w.ops.Log("vfs uploaded %s (%d bytes)", server, info.Size())
		w.report("upload", remote, nil)
	}
	w.mu.Lock()
	delete(w.attempts, key)
	delete(w.busyCount, key)
	w.mu.Unlock()
	// The upload read the bytes that existed when it STARTED. If the file
	// changed while it ran, stamping it in-sync now would clear the dirty bit
	// that edit set — and the newest content would silently never upload
	// (worse: a later dehydrate would destroy its only copy). Leave it dirty
	// and go again.
	if cur, serr := os.Lstat(path); serr != nil {
		w.ops.Log("vfs %s vanished during upload — leaving unmarked", remote)
		return
	} else if !ch.IsDir && (cur.Size() != info.Size() || !cur.ModTime().Equal(info.ModTime())) {
		w.ops.Log("vfs %s changed during upload — re-arming instead of marking in-sync", remote)
		w.reforce(path, forced)
		w.scheduleUpload(path)
		return
	}
	// A FORCED upload is moveServer's 404 fallback: the server never had the
	// source, so the copy just sent at the new name IS the file. The
	// placeholder still carries the identity of that dead source, though, and
	// MarkInSync never rewrites an existing one (it only sets the bit) — the
	// file would end up in sync while naming a server path that no longer
	// exists, so the next "free up space" would dehydrate it and every open
	// after that would hydrate from a 404. Repoint it the way finishRename
	// does: the identity and MARK_IN_SYNC in one call.
	if forced && !ch.IsDir {
		if id, ierr := cfPlaceholderIdentity(path); ierr == nil && len(id) > 0 &&
			!strings.EqualFold(string(id), server) {
			if uerr := cfUpdateIdentity(path, []byte(server)); uerr != nil {
				w.ops.Log("vfs repoint identity %s: %v", path, uerr)
			}
			return
		}
	}
	if err := cfMarkInSync(path, []byte(server)); err != nil {
		w.ops.Log("vfs mark in-sync %s: %v", path, err)
	}
}

// requestHydration queues a pinned, online-only file for download. Callers are
// event handlers and reconcile passes, which must never wait on a transfer, so
// this only ever enqueues, at most one entry per path.
//
// Nothing is ever dropped. A bounded queue used to drop the surplus on the
// assumption that a later reconcile pass meets a still-pinned file again — it
// does not: a pin is a LOCAL attribute change, so the directory's collection
// ETag never moves and the subtree skip returns before the file is walked
// again. Measured: 268 pinned files hydrated 258 on the first pass and 0 on
// every pass after it, leaving "Always keep on this device" stuck on "sync
// pending" until the next app restart. A waiting request is one path string;
// the thing worth avoiding was 50k parked goroutines, not 50k strings.
func (w *Watcher) requestHydration(path string) {
	key := strings.ToLower(path)
	w.mu.Lock()
	if w.hydrating[key] || time.Now().Before(w.hydrateNext[key]) {
		w.mu.Unlock()
		return // already pending/running, or backing off from a failure
	}
	w.hydrating[key] = true
	w.hydratePending = append(w.hydratePending, path)
	w.mu.Unlock()
	w.wakeHydrator()
}

// wakeHydrator nudges one sleeping hydrateLoop. The channel holds a single
// token: if one is already waiting, a worker is about to look at the pending
// list anyway and will see whatever was just added to it.
func (w *Watcher) wakeHydrator() {
	select {
	case w.hydrateWake <- struct{}{}:
	default:
	}
}

// nextHydration takes the oldest pending request, if any, and passes the nudge
// on when work is left over — so both workers end up busy rather than one
// draining the whole list alone.
func (w *Watcher) nextHydration() (string, bool) {
	w.mu.Lock()
	if len(w.hydratePending) == 0 {
		w.mu.Unlock()
		return "", false
	}
	path := w.hydratePending[0]
	w.hydratePending = w.hydratePending[1:]
	more := len(w.hydratePending) > 0
	w.mu.Unlock()
	if more {
		w.wakeHydrator()
	}
	return path, true
}

// hydrateLoop is one of the hydrateWorkers drainers of the pending list,
// started by New (and by the test helper). It exits when the watcher is
// closed.
func (w *Watcher) hydrateLoop() {
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-w.hydrateWake:
		}
		for {
			if w.ctx.Err() != nil {
				return
			}
			path, ok := w.nextHydration()
			if !ok {
				break
			}
			w.hydratePinned(path)
		}
	}
}

// hydratePinned downloads a pinned, online-only file — the provider's half of
// "Always keep on this device". Explorer's own verb hydrates as it pins, but
// Nimbo's context-menu entry and pins applied while Nimbo wasn't running only
// set the attribute (issue #7: pinned folders sat on "sync pending" for a
// day). This is the hydrateLoop worker body: it blocks for the length of the
// download, so everything else goes through requestHydration. A repeatedly
// failing path backs off (30s doubling to 30 minutes, same curve as
// retryLater) instead of being retried and re-reported on every reconcile
// pass. The download itself is NOT reported on success — the provider's own
// hydrate callback reports every download it serves, and reporting here too
// showed the user the same transfer twice.
func (w *Watcher) hydratePinned(path string) {
	key := strings.ToLower(path)
	defer func() {
		w.mu.Lock()
		delete(w.hydrating, key)
		w.mu.Unlock()
	}()
	w.mu.Lock()
	backedOff := time.Now().Before(w.hydrateNext[key])
	w.mu.Unlock()
	if backedOff || w.ctx.Err() != nil {
		return
	}
	remote := w.remoteFor(path)
	ok, err := cfHydrateIfPinned(path)
	if err != nil {
		w.mu.Lock()
		w.hydrateFails[key]++
		n := w.hydrateFails[key]
		next := time.Now().Add(retryDelay(n))
		w.hydrateNext[key] = next
		w.mu.Unlock()
		if n == 1 {
			w.ops.Log("vfs keep on device %s: %v", remote, err)
			w.report("download", remote, err)
		} else {
			w.ops.Log("vfs keep on device %s: attempt %d failed, retrying at %s: %v", remote, n, next.Format(time.RFC3339), err)
		}
		return
	}
	w.mu.Lock()
	delete(w.hydrateFails, key)
	delete(w.hydrateNext, key)
	w.mu.Unlock()
	if ok {
		w.ops.Log("vfs downloaded %s (kept on this device)", remote)
	}
}

// retryLater re-arms a failed server op with per-path exponential backoff.
func (w *Watcher) retryLater(path, key string) {
	w.mu.Lock()
	w.attempts[key]++
	n := w.attempts[key]
	w.mu.Unlock()
	w.scheduleUploadAfter(path, retryDelay(n))
}

// isFileBusy reports a LOCAL sharing/lock violation — the file is open for
// exclusive use by another process (typically: still being copied in).
func isFileBusy(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}

// handleDelete propagates a deletion to the server, unless the path reappeared
// (a rename source or atomic-save shuffle) before the debounce elapsed.
func (w *Watcher) handleDelete(path string) {
	// Running from here to the end, whichever way it ends: a folder's delete
	// waits on this (deletePendingBeneath).
	dkey := strings.ToLower(path)
	w.markDeleting(dkey, true)
	defer w.markDeleting(dkey, false)
	if w.ctx.Err() != nil {
		return
	}
	if w.isSuppressed(path) {
		return // we removed this ourselves during down-sync; already gone server-side
	}
	if w.recentlyMoved(path) {
		w.ops.Log("vfs delete %s skipped: it was moved, not deleted", w.remoteFor(path))
		return
	}
	if w.moveInFlightOrAbove(path) {
		// A MOVE onto this path, or one of its folders, has not landed yet:
		// until it has, the server cannot answer what a delete would remove.
		w.scheduleDeleteAfter(path, deleteDebounce)
		return
	}
	// An upload still running (or queued) for this path is now moot — and left
	// alone it would RECREATE the file on the server after our DELETE, which
	// reconcile then resurrects locally.
	w.cancelInflight(path)
	if _, err := os.Lstat(path); err == nil {
		return // came back — not a real delete
	}
	remote := w.remoteFor(path)
	if remote == "" {
		return // never delete the sync root
	}
	key := strings.ToLower(path)
	// The path is already gone, so we cannot tell whether it was a directory.
	// Assume a file: encoding only ever rewrites a name that is both opted-in and
	// server-forbidden, and sending the RAW name for a disguised FILE is the
	// damaging case — the DELETE 404s, the server copy is orphaned, and the next
	// reconcile pulls it straight back down as a resurrect loop. A directory named
	// exactly ".htaccess" is pathological by comparison.
	server := w.serverFor(path, false)
	// A folder's delete waits for its contents' own deletes. They decide
	// whether anything inside is kept, and a folder DELETE that races its
	// children's is refused by the server as 423 Locked, which dropped the
	// user's delete (test VM, 2026-09-19).
	if w.deletePendingBeneath(path) {
		w.scheduleDeleteAfter(path, deleteDebounce)
		return
	}
	if w.keptBeneath(key) {
		w.keepOnServer(path, remote, "something inside it was kept on the server")
		return
	}
	verdict, why, lerr := w.judgeDelete(server)
	if w.ctx.Err() != nil {
		return
	}
	switch verdict {
	case deleteGone:
		// The server no longer has it: nothing to delete and nothing to retry.
		w.clearDeleteState(key)
		return
	case deleteUnknown:
		// Unknown is not empty: a network blip or a listing that timed out must
		// neither delete blind nor drop the user's delete. Ask again later, and
		// say so once it has failed a few times, as a failed DELETE would.
		w.ops.Log("vfs delete %s: could not check the server copy first: %v", server, lerr)
		w.mu.Lock()
		w.delAttempts[key]++
		n := w.delAttempts[key]
		w.mu.Unlock()
		if n == 3 {
			w.report("delete-remote", remote, fmt.Errorf("could not check what the server holds under it: %w", lerr))
		}
		w.scheduleDeleteAfter(path, retryDelay(n))
		return
	case deleteKeep:
		w.keepOnServer(path, remote, why)
		return
	}
	// The walk can take a while (a guard slot, many listings). Whatever was
	// true before it may not be now: the path re-created and uploaded, a late
	// rename report, our own removal.
	if _, err := os.Lstat(path); err == nil || w.isSuppressed(path) || w.recentlyMoved(path) {
		w.clearDeleteState(key)
		return
	}
	if w.moveInFlightOrAbove(path) {
		w.scheduleDeleteAfter(path, deleteDebounce)
		return
	}
	if err := w.ops.Delete(w.ctx, server); err != nil {
		w.ops.Log("vfs delete %s: %v", server, err)
		w.report("delete-remote", remote, err)
		// A dropped delete resurrects the files at the next reconcile — retry
		// transient failures with the same backoff uploads get.
		if w.ctx.Err() == nil && transport.Retryable(err) {
			w.mu.Lock()
			w.delAttempts[key]++
			n := w.delAttempts[key]
			w.mu.Unlock()
			w.scheduleDeleteAfter(path, retryDelay(n))
		}
		return
	}
	w.clearDeleteState(key)
	w.ops.Log("vfs deleted %s", server)
	w.report("delete-remote", remote, nil)
}

func (w *Watcher) clearDeleteState(key string) {
	w.mu.Lock()
	delete(w.delAttempts, key)
	delete(w.attempts, key)
	delete(w.busyCount, key)
	// A kept folder at or under a path that is now gone for good no longer
	// needs to hold its ancestors back.
	prefix := key + string(filepath.Separator)
	for k := range w.kept {
		if k == key || strings.HasPrefix(k, prefix) {
			delete(w.kept, k)
		}
	}
	w.mu.Unlock()
}

// The delete guard.
//
// On an on-demand mount a folder nobody has opened is EMPTY on disk while the
// server holds all of it, so a single RemoveDirectory succeeds on it at once:
// a script, an "empty folder" cleaner, a backup tool pruning what looks empty,
// or Explorer's own Delete, which removes an online-only folder outright
// without opening it. A DAV DELETE on a collection is recursive, so reading
// that disappearance as "the user deleted it" took everything under it off the
// server, none of it ever seen here (GitHub #7; reproduced on the test VM
// 2026-09-19). The path is gone by the time we hear of it, so the guard judges
// what a DELETE would remove: every file in the server subtree must have been
// on this computer in the version the server holds now (a recorded baseline
// equal to the listing's ETag, which every placeholder, pull and upload
// leaves), or the folder is kept on the server and put back here. A baseline
// alone is not enough: an old one outlives a folder deleted and re-created
// elsewhere, and would vouch for a file of the same name this computer never
// had.

type deleteVerdict int

const (
	deleteSafe    deleteVerdict = iota // everything under it was here: send the DELETE
	deleteGone                         // the server no longer has it
	deleteKeep                         // it holds things never on this computer
	deleteUnknown                      // the server could not be asked: try again later
)

var (
	// maxDeleteCheckListings bounds how many server folders the guard lists
	// for one delete before it gives up and keeps the folder.
	maxDeleteCheckListings = 500
	// deleteCheckConcurrency bounds guard listings across all deletes, so a
	// bulk removal cannot turn into a PROPFIND storm (the 2026-07-03 server
	// DoS was one).
	deleteCheckConcurrency = 4
	// keptMemory is how long a kept folder also keeps its ancestors: a tool
	// that removes empty folders bottom-up removes the parent next.
	keptMemory = 30 * time.Minute
)

// judgeDelete walks the server subtree a DELETE of server would remove.
func (w *Watcher) judgeDelete(server string) (deleteVerdict, string, error) {
	list := w.ops.CheckList
	if list == nil {
		list = w.ops.List
	}
	if list == nil {
		return deleteSafe, "", nil // no listing wired (tests only; the app always has one)
	}
	sem := w.guardSlot()
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-w.ctx.Done():
		return deleteUnknown, "", w.ctx.Err()
	}
	queue := []string{w.listRel(server)}
	for n := 0; len(queue) > 0; n++ {
		if n >= maxDeleteCheckListings {
			return deleteKeep, fmt.Sprintf("it holds more than %d folders on the server, too many to check one by one", maxDeleteCheckListings), nil
		}
		if err := w.ctx.Err(); err != nil {
			return deleteUnknown, "", err
		}
		dir := queue[0]
		queue = queue[1:]
		kids, err := list(dir)
		if err != nil {
			if errors.Is(err, transport.ErrNotFound) {
				if n == 0 {
					return deleteGone, "", nil
				}
				continue // a subfolder went meanwhile: nothing of it left to lose
			}
			return deleteUnknown, "", err
		}
		for _, k := range kids {
			if k.Encrypted {
				return deleteKeep, "it holds an end-to-end encrypted folder, whose contents this computer cannot see", nil
			}
			if k.IsDir {
				queue = append(queue, dir+"/"+k.Name)
				continue
			}
			if base, ok := w.baselineFor(string(k.Identity)); !ok || base == "" || base != k.ETag {
				return deleteKeep, "the server holds files in it that were never on this computer", nil
			}
		}
	}
	return deleteSafe, "", nil
}

// listRel turns a RAW server path into the sync-root-relative form List takes.
func (w *Watcher) listRel(server string) string {
	s := strings.Trim(server, "/")
	rr := strings.Trim(w.remoteRoot, "/")
	if rr == "" {
		return s
	}
	return strings.TrimPrefix(strings.TrimPrefix(s, rr), "/")
}

func (w *Watcher) guardSlot() chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.guardSem == nil {
		w.guardSem = make(chan struct{}, deleteCheckConcurrency)
	}
	return w.guardSem
}

func (w *Watcher) markDeleting(key string, on bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.markDeletingLocked(key, on)
}

// markDeletingLocked counts runs, not paths: two overlapping runs for the
// same path must not let the first to finish clear the second's mark.
func (w *Watcher) markDeletingLocked(key string, on bool) {
	if w.deleting == nil {
		w.deleting = map[string]int{}
	}
	if on {
		w.deleting[key]++
	} else if w.deleting[key] <= 1 {
		delete(w.deleting, key)
	} else {
		w.deleting[key]--
	}
}

// deletePendingBeneath reports whether a delete is waiting or running for
// anything strictly inside path.
func (w *Watcher) deletePendingBeneath(path string) bool {
	prefix := strings.ToLower(path) + string(filepath.Separator)
	w.mu.Lock()
	defer w.mu.Unlock()
	for p := range w.delete {
		if strings.HasPrefix(strings.ToLower(p), prefix) {
			return true
		}
	}
	for k := range w.deleting {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// deletePendingAtOrBeneath reports whether a delete is waiting or running for
// path itself or anything inside it. Reconcile must not pull such a path back
// down: the delete would then find it "came back" and drop.
func (w *Watcher) deletePendingAtOrBeneath(path string) bool {
	key := strings.ToLower(path)
	prefix := key + string(filepath.Separator)
	w.mu.Lock()
	defer w.mu.Unlock()
	for p := range w.delete {
		if k := strings.ToLower(p); k == key || strings.HasPrefix(k, prefix) {
			return true
		}
	}
	for k := range w.deleting {
		if k == key || strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// keptBeneath reports whether something strictly inside the (lower-cased)
// path was kept on the server recently. Deleting the folder would take it.
func (w *Watcher) keptBeneath(key string) bool {
	prefix := key + string(filepath.Separator)
	w.mu.Lock()
	defer w.mu.Unlock()
	for k, at := range w.kept {
		if time.Since(at) > keptMemory {
			delete(w.kept, k)
			continue
		}
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// keepOnServer answers a refused delete: the folder stays on the server, its
// placeholder is put back so this computer shows what the server has, and the
// activity feed says so. It can still be deleted on the server.
func (w *Watcher) keepOnServer(path, remote, why string) {
	key := strings.ToLower(path)
	w.mu.Lock()
	delete(w.delAttempts, key)
	if w.kept == nil {
		w.kept = map[string]time.Time{}
	}
	w.kept[key] = time.Now()
	w.mu.Unlock()
	// Only a folder is ever kept (a file has nothing beneath it), and folder
	// names are never escaped.
	server := w.serverFor(path, true)
	back := "it has been put back"
	if err := w.restorePlaceholder(path, server); err != nil {
		// A full pass lists every folder and pulls it back; ask for one now.
		w.noteEventLoss()
		back = "it comes back on the next full check"
		w.ops.Log("vfs keep %s: put back: %v", server, err)
	}
	w.ops.Log("vfs kept %s on the server: it disappeared from this computer, but %s; %s", server, why, back)
	w.report("delete-kept", remote, nil)
}

// restorePlaceholder puts a kept folder's placeholder back. It comes back
// unpopulated, exactly like a folder reconcile pulls, and fills in when opened.
func (w *Watcher) restorePlaceholder(path, server string) error {
	parent := filepath.Dir(path)
	if _, err := os.Lstat(parent); err != nil {
		return err // the parent went too: its own delete keeps it
	}
	info := cfapi.PlaceholderInfo{Name: filepath.Base(path), IsDir: true, ModTime: time.Now(), Identity: []byte(server)}
	if err := cfCreatePlaceholders(parent, []cfapi.PlaceholderInfo{info}); err != nil {
		if _, serr := os.Lstat(path); serr == nil {
			return nil // reconcile got there first
		}
		return err
	}
	// The window it was deleted from will not show it again on its own
	// (measured on the test VM: invisible until F5).
	cfShellCreated(path, true)
	return nil
}

// handleRename moves the item on the server and repoints the placeholder's
// identity so hydration still works. If the source was never uploaded (MOVE
// fails), it falls back to uploading the destination.
func (w *Watcher) handleRename(oldPath, newPath string) {
	if w.ctx.Err() != nil {
		return
	}
	if w.isSuppressed(oldPath) || w.isSuppressed(newPath) {
		return // a rename we applied during down-sync — the server already has it
	}
	if w.remoteFor(oldPath) == "" || w.remoteFor(newPath) == "" {
		return
	}
	// A rename cannot change dir-ness, so the destination (which exists) settles
	// it for both ends. Each end is encoded independently: renaming notes.txt to
	// .htaccess is a MOVE from the raw name to the escaped one.
	isDir := false
	if fi, err := os.Lstat(newPath); err == nil {
		isDir = fi.IsDir()
	}
	// An upload mid-flight for either end is superseded: the old path's upload
	// would recreate the old name on the server; the new path re-uploads later
	// if the MOVE can't do the job.
	w.cancelInflight(oldPath)
	w.moveServer(w.serverFor(oldPath, isDir), newPath, isDir)
}

// renameMaxRetries bounds MOVE retries — beyond it the rename is reported and
// left for reconcile to sort out rather than retried forever.
const renameMaxRetries = 8

// moveOutcome reports how moveServer's MOVE attempt resolved.
type moveOutcome int

const (
	moveDone       moveOutcome = iota // the server copy is now at the destination
	moveRetrying                      // a retry is scheduled, or retries were exhausted
	moveSourceGone                    // 404: the source was never on the server; handled (uploaded or reported) — not retried as a MOVE
)

// moveServer asks the server to MOVE src (a RAW server path) to newPath's
// server name, with the handling a rename needs: a lost response the server
// applied anyway, transient failures retried as MOVEs, and a 404 source that
// means "never uploaded" — in which case the destination uploads instead
// (forced past the in-sync gate, since reconcile already knows it needs
// nothing done), unless it is an online-only stub, whose only data was at
// src and is now unrecoverable from here.
func (w *Watcher) moveServer(src, newPath string, isDir bool) moveOutcome {
	if w.ctx.Err() != nil {
		// Shutting down. The Move below would fail on the dead ctx anyway, but
		// the lost-response Stat runs on the APP's ctx and can still answer
		// "exists" — finishRename would then walk a tree whose provider may
		// already be disconnected.
		return moveRetrying
	}
	key := strings.ToLower(newPath)
	w.mu.Lock()
	w.inMove[key] = true
	w.mu.Unlock()
	// The mark covers the whole retry CHAIN, not one request: a reconcile pass
	// landing in a retry gap reads the very same evidence a live move does (an
	// identity naming another server path) and would issue a SECOND MOVE for
	// this file. Whichever lands second 404s — a bogus "missing on the server"
	// error for an online-only file, a redundant full re-upload for a hydrated
	// one. So every branch that RESOLVES the move clears it through done(),
	// and the branch that schedules a retry deliberately leaves it set.
	done := func(out moveOutcome) moveOutcome {
		w.mu.Lock()
		delete(w.inMove, key)
		w.mu.Unlock()
		return out
	}
	dst := w.serverFor(newPath, isDir)
	err := w.ops.Move(w.ctx, src, dst)
	if err == nil {
		w.finishRename(src, newPath, dst)
		return done(moveDone)
	}
	// 404 = the source was never on the server (a brand-new file renamed
	// before its first upload) — uploading the destination IS the answer.
	// Anything else gets smarter handling: the server may have APPLIED the
	// move and only the response was lost (falling back to upload would
	// duplicate the file and orphan the old server copy), or the failure
	// is a blip worth retrying as a MOVE.
	if transport.StatusCode(err) == 404 || strings.Contains(err.Error(), "server returned 404") {
		if fi, serr := os.Lstat(newPath); serr == nil && !isDir && cfIsDehydrated(fi, filepath.ToSlash(newPath)) {
			// "Missing on the server" is the truth only when the file really
			// is gone. When the server holds the DESTINATION, this is the
			// repair of a dead identity on a file that is in both places —
			// only its old name is gone — and there is nothing for the user to
			// do or know: the repoint below is the whole fix.
			held := false
			if w.ops.Stat != nil {
				if exists, xerr := w.ops.Stat(dst); xerr == nil && exists {
					held = true
				}
			}
			if held {
				w.ops.Log("vfs move %s -> %s: the source is gone but the server holds the destination — repointing", src, dst)
			} else {
				w.ops.Log("vfs move %s -> %s: source not on server and the local copy is online-only — nothing to upload; the server's Deleted files may still have it", src, dst)
				w.report("move", w.remoteFor(newPath), fmt.Errorf("%s is missing on the server and was never downloaded", w.localName(src)))
			}
			// No local data to save and no server copy to move: repoint the
			// identity to where it would be so the NEXT pass reads this as an
			// ordinary server-side delete (and removes it locally) instead of
			// rediscovering the same foreign identity and re-reporting this
			// same unrecoverable move forever (issue #7's 37 attempts).
			if uerr := cfUpdateIdentity(newPath, []byte(dst)); uerr != nil {
				w.ops.Log("vfs repoint identity %s: %v", newPath, uerr)
			}
			return done(moveSourceGone)
		}
		w.ops.Log("vfs move %s -> %s: source not on server (falling back to upload)", src, dst)
		// The destination is already in-sync as far as cfInspect is concerned
		// (that's WHY it looked like a foreign-identity move rather than a
		// pending upload) — a plain handleChange would see NeedsUpload ==
		// false and no-op, leaving the data stranded. Force it through.
		w.forceChange(newPath)
		return done(moveSourceGone)
	}
	if w.ops.Stat != nil {
		if exists, serr := w.ops.Stat(dst); serr == nil && exists {
			w.ops.Log("vfs move %s -> %s: response lost but the server applied it", src, dst)
			w.finishRename(src, newPath, dst)
			return done(moveDone)
		}
	}
	if w.ctx.Err() != nil {
		return done(moveRetrying)
	}
	w.ops.Log("vfs move %s -> %s: %v (will retry)", src, dst, err)
	w.report("move", w.remoteFor(newPath), err)
	w.mu.Lock()
	w.mvAttempts[strings.ToLower(newPath)]++
	n := w.mvAttempts[strings.ToLower(newPath)]
	w.mu.Unlock()
	if n > renameMaxRetries {
		w.ops.Log("vfs move %s -> %s: giving up after %d attempts", src, dst, n)
		return done(moveRetrying)
	}
	time.AfterFunc(retryDelay(n), func() {
		if w.ctx.Err() != nil {
			done(moveRetrying) // nothing will retry this now: release the mark
			return
		}
		w.moveServer(src, newPath, isDir)
	})
	return moveRetrying // the mark stays: this move is not resolved yet
}

// moveInFlight reports whether a server MOVE onto path is running right now.
func (w *Watcher) moveInFlight(path string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.inMove[strings.ToLower(path)]
}

// moveInFlightOrAbove reports whether a server MOVE is running on path or on
// any directory ABOVE it, up to the sync root.
//
// The single-key answer is not enough for a deferred placement check. A file
// saved INSIDE a directory whose MOVE is still running (a big folder on
// Nextcloud takes seconds — the filecache is rewritten per descendant) carries
// an identity naming the OLD parent, which reads exactly like a file owed a
// move of its own. Moving it there puts it at a path the parent's MOVE is
// about to take with Overwrite: T: either the child's server copy and the edit
// just uploaded into it go to the trash, or the parent's MOVE fails and the
// lost-response Stat finds the destination collection our own MOVE pre-created
// and reads the failure as applied. Both silent.
//
// Waiting is always right instead: finishRename's repointTree stamps every
// descendant before the parent's mark clears, so the check then finds the
// child already repointed and no-ops — and if the parent's chain outlives
// renameVerifyMax, repointTree's own scheduleUploadIfIdle carries the edit.
func (w *Watcher) moveInFlightOrAbove(path string) bool {
	root := strings.ToLower(w.root)
	w.mu.Lock()
	defer w.mu.Unlock()
	for p := path; strings.HasPrefix(strings.ToLower(p), root); {
		if w.inMove[strings.ToLower(p)] {
			return true
		}
		parent := filepath.Dir(p)
		if parent == p {
			break // at the volume root: nothing above it
		}
		p = parent
	}
	return false
}

// finishRename records a rename the server now agrees with. src is the RAW
// server path the item moved from.
func (w *Watcher) finishRename(src, newPath, dst string) {
	w.mu.Lock()
	delete(w.mvAttempts, strings.ToLower(newPath))
	w.mu.Unlock()
	w.ops.Log("vfs moved %s -> %s", src, dst)
	w.report("move", w.remoteFor(newPath), nil)
	isDir := false
	if fi, err := os.Lstat(newPath); err == nil {
		isDir = fi.IsDir()
	}
	// The server copy is the same version under a new name: its conflict
	// baseline travels with it, and so does every descendant's (gathered by
	// repointTree below). They are carried in ONE write at the end: the store
	// persists its whole file per call, so a per-item carry turned a
	// thousand-file folder move into thousands of full rewrites of a
	// multi-megabyte JSON, all inside the window the move's in-flight mark
	// makes every placement check wait on.
	pairs := [][2]string{{src, dst}}
	// Uploads are scheduled only after that write, so a kept edit's upload
	// reads the baseline of its new name rather than racing the carry.
	var pending []string
	// The identity must name the file as the SERVER knows it, or hydration 404s.
	// An edit made just before the move is still waiting to upload, and
	// handleRename has already cancelled the queued upload of the old path:
	// stamping this one in-sync as well would leave the edit invisible to
	// everything, and the next refresh would dehydrate the only copy of it
	// away. Repoint such a file without touching that state and send it;
	// everything else is stamped in-sync, which also restores the bit the
	// move itself cleared (see unsyncedContent). A directory never takes the
	// keep-state branch: it has no content of its own, and the upload it would
	// schedule is a pointless MKCOL of a folder the MOVE just created.
	if !isDir && w.unsyncedContent(newPath) {
		if err := cfUpdateIdentityKeep(newPath, []byte(dst)); err != nil {
			w.ops.Log("vfs repoint identity %s: %v", newPath, err)
		}
		pending = append(pending, newPath)
	} else if err := cfUpdateIdentity(newPath, []byte(dst)); err != nil {
		w.ops.Log("vfs repoint identity %s: %v", newPath, err)
	}
	if isDir {
		descendants, dirty := w.repointTree(newPath)
		pairs = append(pairs, descendants...)
		pending = append(pending, dirty...)
	}
	w.moveBaselines(pairs)
	for _, p := range pending {
		w.scheduleUploadIfIdle(p)
	}
}

// unsyncedContent reports whether a file holds LOCAL content the server has
// not got, which is what "dirty" must mean around a move.
//
// The cloud filter clears a placeholder's in-sync bit on every rename or move
// by another process (measured live 2026-09-15 — an online-only stub moved
// between folders went InSyncState 1 -> 0 with no modified data at all), so
// the bit cannot tell a pending edit from the move itself. The placeholder's
// modified-data size can: a hydrated file written locally reported 4096 of
// 4096 bytes modified, before and after its own move.
//
// cfInspect asks the same question the same way since fix wave 3 (the bit is
// its pre-filter, not its verdict), so the two now agree; this stays a
// separate call because the move path wants the answer for a specific FILE
// without re-deriving the rest of a Change. An unreadable file counts as clean
// — the same answer the old cfInspect error path gave, and the repoint that
// follows fails just as visibly.
func (w *Watcher) unsyncedContent(path string) bool {
	mod, err := cfPlaceholderModified(path)
	if err != nil {
		w.ops.Log("vfs modified-data check %s: %v", path, err)
		return false
	}
	return mod
}

// repointTree rewrites the identity of every placeholder beneath a directory
// that was just renamed or moved: the server MOVE changed all their paths, and
// an online-only child whose identity still names the old path would 404 on
// its first open. Plain (never-uploaded) items are left alone.
//
// Each descendant takes the same decision finishRename takes for the moved
// item itself: a FILE holding unsynced local content is repointed WITHOUT
// MARK_IN_SYNC and sent, because stamping it in-sync clears the dirty bit and
// the next refresh or "free up space" then dehydrates the only copy of that
// edit away. Directories have no content of their own and are always stamped.
//
// It returns the baseline carries the walk earned (old server path -> new one)
// and the files still owed an upload, both for the caller to apply once: the
// baseline store persists its whole file per call, and a kept upload must not
// run before its baseline has arrived under the new name.
func (w *Watcher) repointTree(dir string) (pairs [][2]string, pending []string) {
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == dir || w.ctx.Err() != nil {
			return nil
		}
		fi, ierr := d.Info()
		if ierr != nil || !cfIsPlaceholder(fi, filepath.ToSlash(p)) {
			return nil
		}
		id := []byte(w.serverFor(p, d.IsDir()))
		// The identity still names the pre-move server path: that is where
		// this descendant's conflict baseline is recorded, so it has to be
		// read before the identity is rewritten (see moveBaseline).
		if old, oerr := cfPlaceholderIdentity(p); oerr == nil && len(old) > 0 {
			pairs = append(pairs, [2]string{string(old), string(id)})
		}
		if !d.IsDir() && w.unsyncedContent(p) {
			if uerr := cfUpdateIdentityKeep(p, id); uerr != nil {
				w.ops.Log("vfs repoint identity %s: %v", p, uerr)
			}
			pending = append(pending, p)
			return nil
		}
		if uerr := cfUpdateIdentity(p, id); uerr != nil {
			w.ops.Log("vfs repoint identity %s: %v", p, uerr)
		}
		return nil
	})
	return pairs, pending
}

// --- Down-sync: pull server changes into populated directories ---

// Reconcile makes one pass over the populated directory tree, pulling in remote
// additions and removals. Safe to call concurrently (passes are serialised).
func (w *Watcher) Reconcile() {
	if w.ops.List == nil {
		return
	}
	if !w.reconMu.TryLock() {
		return // a pass is already running
	}
	defer w.reconMu.Unlock()
	// The first pass after start ignores the ETag subtree skip. The skip assumes
	// local state is good wherever the server is unchanged — but #580 flattening
	// (placeholders losing their cloud state across a provider shutdown) breaks
	// exactly that assumption, and healPlainFile can only run where a listing
	// happens. One full sweep per start bounds the extra cost at the pre-skip
	// steady state, once.
	// Lost events (buffer overflow, a reopened watch) also force a skip-free
	// pass: a local change in the gap invalidates the skip's assumption.
	w.fullSweep = !w.firstPassDone || w.lostEvents.Swap(false)
	if w.reconcileDir("", "") && w.fullSweep {
		w.firstPassDone = true
	}
	w.fullSweep = false
}

// reconcileDir reconciles one already-populated directory (rel; "" = root) and
// recurses into its populated subdirectories. Unpopulated (lazy) directories are
// left untouched — they fetch fresh when opened.
//
// knownETag is the directory's current server collection ETag (from the parent's
// listing; "" for the root, which is always listed). Nextcloud propagates a
// collection's ETag up the tree on any descendant change, so when knownETag
// matches the ETag we last reconciled this directory at, nothing in it OR its
// subtree changed and we skip it entirely — a safety-net poll over a large, idle
// populated tree then costs one PROPFIND of the root instead of one per dir.
// It returns whether the directory and its whole subtree were fully reconciled;
// the caller records knownETag only on success, so a transient failure isn't
// masked by the subtree skip on the next pass.
func (w *Watcher) reconcileDir(rel, knownETag string) bool {
	if w.ctx.Err() != nil {
		return false
	}
	dirRemote := strings.Trim(w.remoteRoot+"/"+rel, "/")
	if knownETag != "" && !w.fullSweep {
		if base, ok := w.baselineFor(dirRemote); ok && base == knownETag {
			return true // unchanged subtree (ETag propagation guarantees nothing below changed)
		}
	}
	localDir := w.root
	if rel != "" {
		localDir = filepath.Join(w.root, filepath.FromSlash(rel))
	}
	entries, err := os.ReadDir(localDir)
	if err != nil {
		// A corrupt DIRECTORY placeholder fails here, and nothing else in
		// this pass ever opens it — so without this it is the one class of
		// broken entry that stays completely silent while re-listing its
		// parent on every pass. Files reach noteCorrupt through their
		// identity reads below; other errors are transient and stay quiet.
		w.noteCorrupt(localDir, err)
		return false // can't read locally — don't claim reconciled
	}
	if len(entries) == 0 && rel != "" {
		// An empty directory is either LAZY — never populated, so the shell
		// will issue FETCH_PLACEHOLDERS the first time it is opened and
		// pulling its children here would race that transfer — or genuinely
		// EMPTY and already populated, in which case the shell will NEVER ask
		// again: the populated flag is permanent and the transfer sets it
		// whenever the listing succeeded, zero entries included. Counting
		// entries cannot tell those apart, and reading the empty one as lazy
		// is how a folder that later gains content on another device stayed
		// invisible here for good (VM, build 0.1.0.284: /Notes, populated
		// empty at attrs 0x100410, never showed the subfolder added
		// elsewhere). The directory's own attribute answers it exactly.
		//
		// The ROOT is never lazy — we seed its top level ourselves
		// (DisableOnDemandPopulationOnRoot) — so it is always listed.
		populated, perr := cfDirPopulated(localDir)
		if perr != nil {
			w.ops.Log("vfs reconcile %q: population state: %v", rel, perr)
			return false // unknown — don't claim this subtree reconciled
		}
		if !populated {
			return true // lazy: the shell owns its first fetch
		}
	}
	remote, err := w.ops.List(rel)
	if err != nil {
		w.ops.Log("vfs reconcile list %q: %v", rel, err)
		return false // unknown listing — never treat as "all deleted"
	}
	// nameKey normalises a name for MATCHING only — never for an actual operation,
	// which always uses the name verbatim. Windows filenames are case-insensitive,
	// so a server entry differing only in case IS the local file; matching it
	// case-sensitively makes it look like a missing addition on every pass, and
	// the entry is then deleted locally and re-created in an endless churn
	// (CfCreatePlaceholders reporting ERROR_ALREADY_EXISTS). skipName already
	// learned this: "a live Desktop.ini once slipped past an exact match".
	nameKey := strings.ToLower

	remoteByName := make(map[string]cfapi.PlaceholderInfo, len(remote))
	for _, r := range remote {
		remoteByName[nameKey(r.Name)] = r
	}

	// Pre-pass: which names exist locally, and which remote files are additions
	// (no local placeholder by that name) keyed by oc:fileid — the candidates a
	// server rename could have produced.
	// The server listing is keyed by RAW names, so local names must be encoded
	// before they are matched against it — a disguised file is ".htaccess" on disk
	// and ".htaccess.nimboesc" on the server. Without this the local file matches
	// nothing, and (being an in-sync placeholder) is deleted and recreated under
	// the escaped name.
	serverName := func(e os.DirEntry) string {
		if w.ops.Encode == nil || e.IsDir() {
			return e.Name()
		}
		return w.ops.Encode(e.Name())
	}
	localByName := map[string]bool{}
	for _, e := range entries {
		if !skipName(e.Name()) {
			localByName[nameKey(serverName(e))] = true
		}
	}
	addByFileID := map[string]cfapi.PlaceholderInfo{}
	for _, r := range remote {
		if !r.IsDir && r.FileID != "" && !localByName[nameKey(r.Name)] {
			addByFileID[r.FileID] = r
		}
	}
	consumed := map[string]bool{} // remote additions claimed as a rename target
	// dirOK gates recording this directory's collection ETag: a failed per-item
	// operation (remove, refresh, create) must NOT be masked by the subtree
	// skip until the next restart — the pass has to come back here.
	dirOK := true

	type subdir struct{ rel, etag string }
	var subdirs []subdir
	for _, e := range entries {
		name := e.Name()
		if skipName(name) {
			// Sync-excluded local artifacts (official-client journals,
			// Desktop.ini) are plain files, which Explorer renders as
			// forever-pending inside a cloud root. Mark them EXCLUDED —
			// the platform's own "not synced, on purpose", which draws a
			// blank Status cell. Idempotent; never touches content.
			if !e.IsDir() {
				if xerr := cfExclude(filepath.Join(localDir, name)); xerr != nil {
					w.ops.Log("vfs exclude %s: %v", name, xerr)
				}
			}
			continue
		}
		full := filepath.Join(localDir, name)
		r, inRemote := remoteByName[nameKey(serverName(e))]
		if !inRemote {
			// Local has it, server doesn't. Only touch an in-sync placeholder
			// (known to mirror the server); a not-in-sync item is a pending
			// upload / brand-new local file and must be kept — AND actually
			// uploaded: if its change event was lost or its upload failed
			// before a restart, nothing else would ever push it (issue #1's
			// "that file specifically is now ignored").
			// A share or mount the listing once marked as its ROOT has been
			// detached from the account, not deleted (Deck #557): keep whatever
			// has bytes and hand it over for parking outside the cloud folder.
			// Checked before the dirty-item rescue: a locally edited file whose
			// share is gone would otherwise be retried as an upload forever.
			if w.ops.MountRoot != nil && w.ops.MountRoot(w.serverFor(full, e.IsDir())) {
				if !w.detach(full, e.IsDir()) {
					dirOK = false
				}
				continue
			}
			ch, ierr := cfInspect(full)
			if ierr != nil {
				w.noteCorrupt(full, ierr)
				continue
			}
			// A placeholder whose identity names a DIFFERENT server path was
			// moved here while the watcher wasn't running (or both live
			// detectors missed it): the server still has it at the old path.
			// MOVE it there — never treat it as a server-side delete, which
			// would remove the only reachable copy from this PC too.
			//
			// This is the THIRD rename detector, and the weakest: it only
			// ever runs on a FULL sweep (the first pass after a start, or a
			// lost-events pass). Everywhere else the ETag subtree skip above
			// returns before the directory is even read, and a placement that
			// drifted locally leaves the server's ETag untouched by
			// definition — so nothing here can notice it. Steady-state drift
			// is verifyPlacement's job; this one is the restart safety net.
			//
			// Checked BEFORE the dirty rescue below, because a file can be
			// both: edited, its upload failed, then moved while Nimbo was
			// closed. Rescuing it first uploads the edit at the NEW path and
			// leaves the server's copy stranded at the old one, which the next
			// pass of the old parent re-creates locally — the user ends up
			// with the file twice. The MOVE handles both halves: finishRename
			// keeps the pending upload and sends it at the new path.
			cid, ciderr := cfPlaceholderIdentity(full)
			if ciderr != nil {
				// Windows cannot read it; nothing below can either. For the
				// 363 fault that is permanent: the RemoveAll further down
				// fails with the same error on every pass, logging a failure
				// and withholding this directory's ETag for as long as the
				// entry exists, which buys nothing. Report it (once) and
				// leave it be — the ETag still gets recorded, so the rest of
				// the directory goes back to the cheap subtree skip.
				if w.noteCorrupt(full, ciderr) {
					continue
				}
			} else if id := cid; len(id) > 0 {
				if want := w.serverFor(full, ch.IsDir); !strings.EqualFold(string(id), want) {
					if w.moveInFlightOrAbove(full) {
						// A live move (the filter's callback, or a REMOVED +
						// ADDED pair) already owns this path and is reading
						// the very same evidence. Racing it with a second
						// MOVE makes one of the two 404.
						//
						// A move running on a directory ABOVE this path counts
						// too: every identity beneath it still names the old
						// parent, which is exactly the evidence here, and
						// finishRename's repointTree is about to rewrite them
						// all. Moving one of them ourselves sends it to the
						// path the parent's MOVE is about to take with
						// Overwrite: T.
						dirOK = false // come back next pass
						continue
					}
					switch w.moveServer(string(id), full, ch.IsDir) {
					case moveRetrying, moveSourceGone:
						// moveRetrying: the MOVE itself hasn't resolved yet.
						// moveSourceGone: the identity was just repointed to
						// where this pass's (now stale) listing still shows
						// nothing — the deferred local cleanup that repoint
						// sets up (or the upload that already ran) must be
						// re-examined against a FRESH listing, not masked by
						// the ETag subtree skip caching this pass's view.
						dirOK = false // come back next pass
					}
					continue
				}
			}
			// Not moved, just never pushed: an edit whose change event was
			// lost or whose upload failed before a restart has no live retry
			// timer, and no event will ever fire for it again. Plain
			// (never-placeholder) files reach this the same way.
			if ch.NeedsUpload {
				w.scheduleUploadIfIdle(full)
				continue
			}
			// Rename detection: if this gone-from-server file's recorded fileid
			// reappears as a server-side addition, the server renamed it. Move the
			// local placeholder (preserving any hydration) instead of delete+create.
			// fileids are recorded under the RAW server path (from the listing's
			// Identity), so they must be read and dropped under the same key —
			// reading with the local name silently misses for a disguised file and
			// degrades a server rename into delete+recreate.
			if !ch.IsDir {
				if fid, ok := w.fileIDFor(w.serverFor(full, false)); ok {
					if y, isRename := addByFileID[fid]; isRename && !consumed[nameKey(y.Name)] {
						if w.pullRename(localDir, full, y, fid) {
							consumed[nameKey(y.Name)] = true
							continue
						}
					}
				}
			}
			// Not a rename — propagate the server-side delete locally.
			w.suppressDelete(full)
			if rerr := os.RemoveAll(full); rerr != nil {
				w.ops.Log("vfs reconcile remove %s: %v", name, rerr)
				dirOK = false
			} else {
				w.ops.Log("vfs pulled delete %s", w.serverFor(full, ch.IsDir))
				w.report("delete-local", w.remoteFor(full), nil) // user-visible name
				w.dropFileID(w.serverFor(full, ch.IsDir))
			}
			continue
		}
		if e.IsDir() {
			// A PLAIN in-both directory is a flatten victim no other heal can
			// reach once it is EMPTY: reconcile treats empty as lazy and never
			// lists it, so it never earns the baseline healPlainDirs demands —
			// while Explorer reads a non-placeholder dir as never-synced and
			// projects pending arrows onto every ancestor. The listing in hand
			// IS server proof: replace an empty one with a real lazy
			// placeholder, convert a populated one in place.
			if r.IsDir {
				if fi, ierr := e.Info(); ierr == nil && !cfIsPlaceholder(fi, full) {
					if kids, rerr := os.ReadDir(full); rerr == nil && len(kids) == 0 {
						w.suppressDelete(full) // our own remove, not the user's
						if derr := os.Remove(full); derr == nil {
							if cerr := cfCreatePlaceholders(localDir, []cfapi.PlaceholderInfo{r}); cerr != nil {
								w.ops.Log("vfs replace plain dir %s: %v", name, cerr)
							} else {
								// Don't stop at a LAZY placeholder — not-in-sync
								// draws the very arrows this heal removes.
								// Reconcile is online: populate from the listing
								// and mark the dir settled.
								childRel := name
								if rel != "" {
									childRel = rel + "/" + name
								}
								if remoteKids, lerr := w.ops.List(childRel); lerr == nil {
									if len(remoteKids) > 0 {
										if kerr := cfCreatePlaceholders(full, remoteKids); kerr != nil {
											w.ops.Log("vfs populate replaced dir %s: %v", name, kerr)
										} else {
											// One write, as for the pull below.
											kbase := make(map[string]string, len(remoteKids))
											for _, k := range remoteKids {
												if k.ETag != "" {
													kbase[string(k.Identity)] = k.ETag
												}
												if !k.IsDir {
													w.recordFileID(string(k.Identity), k.FileID)
												}
											}
											w.recordBaselines(kbase)
										}
									}
									if merr := cfMarkInSync(full, r.Identity); merr != nil {
										w.ops.Log("vfs mark replaced dir %s: %v", name, merr)
									}
								}
								w.ops.Log("vfs replaced empty plain dir %s with a placeholder", w.remoteFor(full))
								cfShellNotify(full)
							}
						}
					} else if merr := cfMarkInSync(full, r.Identity); merr != nil {
						w.ops.Log("vfs convert plain dir %s: %v", name, merr)
					} else {
						w.ops.Log("vfs converted plain dir %s in place", w.remoteFor(full))
						cfShellNotify(full)
					}
				}
			}
			child := name
			if rel != "" {
				child = rel + "/" + name
			}
			subdirs = append(subdirs, subdir{rel: child, etag: r.ETag})
			continue
		}
		fi, statErr := e.Info()
		if statErr != nil {
			w.noteCorrupt(full, statErr)
			continue
		}
		// Present both sides but locally PLAIN — a placeholder that lost its
		// cloud state (mode switches and provider shutdown strip it, Deck #580),
		// which Explorer draws as forever-pending. Heal it if it provably still
		// mirrors the server; either way the refresh below is placeholder-only.
		if !cfIsPlaceholder(fi, full) {
			w.healPlainFile(full, fi, r)
			continue
		}
		// Rescue dirty in-both files on EVERY pass: an edit whose upload failed
		// before a restart — or whose change event was lost to a buffer
		// overflow — has no live retry timer, and no event will ever fire for
		// it again. The ETag subtree skip keeps the steady-state cost down
		// (unchanged subtrees aren't walked at all).
		ch, ierr := cfInspect(full)
		if ierr != nil {
			w.noteCorrupt(full, ierr)
		}
		if ierr == nil && ch.NeedsUpload {
			w.scheduleUploadIfIdle(full)
			continue
		}
		// Clean, but NOT in sync: the bit a rename or move cleared, on a file
		// that turned out to carry no local change. Nothing else will ever
		// look at this item again — the rescue above no longer claims it and
		// no event is coming — so without this the sync-pending arrows on it
		// and on every ancestor folder are permanent.
		//
		// The DATA question is the whole question: the inspect above has
		// already proved there is nothing local to lose. Requiring the
		// server's ETag to still match our baseline as well left the one case
		// it was meant to defer — a server copy that moved on — in a state
		// nothing could act on: the heal declined it, and the refresh below is
		// VERIFY_IN_SYNC, which the driver refuses for a NOT-in-sync
		// placeholder, so every pass logged "skipped: edited locally just
		// now" and changed nothing, forever. Heal first; the refresh then
		// runs with the bit set, and its VERIFY_IN_SYNC still closes the
		// edit-in-between race.
		if ierr == nil && ch.Placeholder && !ch.InSync && !ch.NeedsUpload {
			// The same refusal the event path makes: a placeholder whose
			// identity names another server path is owed a MOVE, and stamping
			// it in sync here would say the opposite. Reachable by moving a
			// file over an existing placeholder inside the deduplication
			// window. Hand it to the placement check instead, exactly as
			// handleChange does.
			if id, iderr := cfPlaceholderIdentity(full); iderr == nil && len(id) > 0 &&
				!strings.EqualFold(string(id), w.serverFor(full, false)) {
				w.ops.Log("vfs %s still names %s: a move owns it; checking where it belongs", w.remoteFor(full), string(id))
				go w.verifyPlacement(full)
			} else if serr := cfSetInSync(full); serr != nil {
				w.ops.Log("vfs restore in-sync %s: %v", name, serr)
			} else {
				w.ops.Log("vfs restored in-sync state for %s (moved, not modified)", w.remoteFor(full))
				cfShellNotify(full)
			}
		}
		// IN sync, clean, and yet the identity names another server path: the
		// dead-identity population every hydrated 404 fallback before fix
		// wave 5 left behind (and every stub "free up space" has made of one
		// since). The guard and the heal above both need the bit clear, so
		// nothing ever looked at these; their next open hydrates from the dead
		// path and 404s, with an error toast per open and no way out. The
		// listing has just said the server holds the file at THIS name, so the
		// repair is a repoint in place — no MOVE (the source is gone, or worse
		// is a duplicate an Overwrite: T would bury), no data access, and the
		// bit stays set. Once per start (the first pass is a full sweep): one
		// identity read per in-sync file is too much for every poll of a large
		// mount, and the population only grows while Nimbo is running through
		// paths that now repoint themselves.
		if w.fullSweep && ierr == nil && ch.Placeholder && ch.InSync && !ch.NeedsUpload && len(r.Identity) > 0 {
			// This is also the one open reconcile makes of a healthy-looking
			// in-sync file (the listing supplied its attributes; nothing else
			// touches it), so it is where an entry Windows cannot read shows
			// itself — and is reported, once.
			id, iderr := cfPlaceholderIdentity(full)
			if iderr != nil {
				w.noteCorrupt(full, iderr)
			} else if len(id) > 0 && !strings.EqualFold(string(id), string(r.Identity)) {
				if w.moveInFlightOrAbove(full) {
					dirOK = false // a live move owns it; look again next pass
				} else if uerr := cfUpdateIdentity(full, r.Identity); uerr != nil {
					w.ops.Log("vfs repoint %s (its identity named %s): %v", w.remoteFor(full), string(id), uerr)
					dirOK = false
				} else {
					// No baseline is recorded here on purpose. The refresh
					// check below compares the listing's ETag against the
					// recorded one; writing the listing's ETag first makes it
					// compare with itself, so a server edit that happens to
					// match on size and mtime would look unchanged and the
					// stale local copy would be recorded as mirroring it. The
					// refresh path records after a real refresh.
					w.ops.Log("vfs repointed %s (its identity named %s)", w.remoteFor(full), string(id))
				}
			}
		}
		// File present both sides: if the server copy changed and our copy is
		// in-sync (clean), refresh it so a previously-downloaded file isn't stale.
		// (A dirty local copy is a pending upload / potential conflict — left alone.)
		if w.remoteChanged(r, fi, string(r.Identity)) {
			// Same inspect as the rescue above (it is a metadata OPEN now for
			// anything not in sync, so it is not re-run per branch).
			if ierr == nil && !ch.NeedsUpload {
				// VERIFY_IN_SYNC closes the race between that inspect and the
				// refresh: an edit landing in between fails the update (and we
				// skip) instead of being dehydrated away.
				if rerr := cfRefreshIfInSync(full, r.Identity, r.Size, r.ModTime); errors.Is(rerr, cfapi.ErrNotInSync) {
					w.ops.Log("vfs refresh %s skipped: edited locally just now", name)
				} else if rerr != nil {
					w.ops.Log("vfs refresh %s: %v", name, rerr)
					dirOK = false
				} else {
					w.ops.Log("vfs refreshed %s (server changed)", w.remoteFor(full))
					w.report("download", w.remoteFor(full), nil)
					w.recordBaseline(string(r.Identity), r.ETag) // now mirrors the new server version
				}
			}
		}
		// Hydrate LAST, after any refresh above: a pinned file with a stale
		// server copy must land the CURRENT content first, so the download
		// isn't done against bytes about to be replaced (or, if the refresh
		// fails, wrongly reported as this pass's only outcome).
		if cfPinnedDehydrated(full) {
			w.requestHydration(full) // pinned while we weren't running
		}
	}

	// Additions: remote entries with no local placeholder yet (minus any claimed
	// as a rename target above).
	//
	// skipName must be applied HERE too, not just to the local side. A junk name
	// present on BOTH sides is filtered out of localByName, so without this it
	// looks like a missing addition on every single pass and CfCreatePlaceholders
	// fails with ERROR_ALREADY_EXISTS forever (seen in the field as a repeating
	// "reconcile create in ...: 0x800700b7"). It also stops server-side junk —
	// an official-client sync log, desktop.ini — being pulled down at all.
	var toCreate []cfapi.PlaceholderInfo
	for _, r := range remote {
		if !skipName(r.Name) && !localByName[nameKey(r.Name)] && !consumed[nameKey(r.Name)] {
			if w.deletePendingAtOrBeneath(filepath.Join(localDir, r.Name)) {
				// Gone here because the user deleted it, and the delete is
				// still being checked or sent. Pulling it back would make the
				// delete read it as "came back" and drop it; look again later.
				dirOK = false
				continue
			}
			toCreate = append(toCreate, r)
		}
	}
	if len(toCreate) > 0 {
		if cerr := cfCreatePlaceholders(localDir, toCreate); cerr != nil {
			dirOK = false
			// Name the items: the bare HRESULT told us nothing when this fired on
			// every poll in the field, and CfCreatePlaceholders' per-item results
			// are not surfaced by the wrapper.
			names := make([]string, 0, len(toCreate))
			for _, r := range toCreate {
				names = append(names, r.Name)
			}
			w.ops.Log("vfs reconcile create in %q: %v (items: %s)", rel, cerr, strings.Join(names, ", "))
		} else {
			w.ops.Log("vfs pulled %d new item(s) into %q", len(toCreate), rel)
			// One write for the whole pull: the baseline store persists its
			// entire file per call, so recording item by item made a
			// thousand-item directory a thousand full rewrites.
			base := make(map[string]string, len(toCreate))
			for _, r := range toCreate {
				if r.ETag != "" {
					base[string(r.Identity)] = r.ETag
				}
				if !r.IsDir {
					w.recordFileID(string(r.Identity), r.FileID) // enable rename detection later
				}
				w.report("download", string(r.Identity), nil) // surface new server files in the activity feed
			}
			w.recordBaselines(base)
		}
	}

	ok := dirOK
	for _, sd := range subdirs {
		if !w.reconcileDir(sd.rel, sd.etag) {
			ok = false
		}
	}
	// Record this directory's ETag only once it AND its whole subtree reconciled,
	// so the subtree-skip above can't hide a transient failure. The root (empty
	// knownETag) is always re-listed.
	if ok && knownETag != "" {
		w.recordBaseline(dirRemote, knownETag)
	}
	return ok
}

// healPlainFile converts a plain file back into an in-sync placeholder when it
// provably still mirrors the server copy: size and mtime equal the listing's
// (the values stamped on the placeholder when it was created) and any recorded
// baseline agrees the server has not moved on since our last sync. Attribute
// surgery only — no transfer, no server ops. Anything short of proof is left
// alone: a mismatch means a local edit the watcher never saw or a newer server
// version, and stamping either "in sync" would be a lie the write-back gate
// then acts on.
func (w *Watcher) healPlainFile(full string, fi os.FileInfo, r cfapi.PlaceholderInfo) {
	if fi.Size() != r.Size {
		return
	}
	if d := fi.ModTime().Sub(r.ModTime); d < -2*time.Second || d > 2*time.Second {
		return
	}
	remote := string(r.Identity)
	if base, ok := w.baselineFor(remote); ok && base != "" && base != r.ETag {
		return // server moved on; matching metadata proves nothing
	}
	if err := cfMarkInSync(full, r.Identity); err != nil {
		w.ops.Log("vfs heal plain file %s: %v", remote, err)
		return
	}
	w.ops.Log("vfs healed plain file %s (matches server)", remote)
	cfShellNotify(full) // or Explorer keeps the stale pending glyph until F5
	w.recordBaseline(remote, r.ETag)
	if r.FileID != "" {
		w.recordFileID(remote, r.FileID)
	}
}

// suppressDelete marks a path (and its subtree) as removed by us, so the
// watcher's own REMOVED notification doesn't bounce back as a server delete.
func (w *Watcher) suppressDelete(path string) {
	w.mu.Lock()
	w.suppress[strings.ToLower(path)] = time.Now()
	w.mu.Unlock()
}

func (w *Watcher) isSuppressed(path string) bool {
	lp := strings.ToLower(path)
	w.mu.Lock()
	defer w.mu.Unlock()
	for p, t := range w.suppress {
		if time.Since(t) > 10*time.Second {
			delete(w.suppress, p)
			continue
		}
		if lp == p || strings.HasPrefix(lp, p+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// detach handles a vanished share or mount root (Deck #557): the local copy is
// salvaged in place and handed to Ops.Detached, which parks it outside the
// cloud folder. A copy with nothing in it worth keeping — every file an
// online-only stub — is removed, and said so. Never a server delete. Returns
// false when the folder must be looked at again next pass.
func (w *Watcher) detach(full string, isDir bool) bool {
	remote := w.remoteFor(full)        // the user-visible name
	server := w.serverFor(full, isDir) // what the stores are keyed by
	w.suppressDelete(full)             // everything below is our doing, not the user's
	kept, err := w.salvage(full)
	if err != nil {
		w.ops.Log("vfs no longer shared: salvage %s: %v", remote, err)
		return false
	}
	if !kept {
		if rerr := os.RemoveAll(full); rerr != nil {
			w.ops.Log("vfs no longer shared: remove %s: %v", remote, rerr)
			return false
		}
		w.dropFileID(server)
		w.forget(server)
		w.ops.Log("vfs %s is no longer shared with you; nothing was downloaded, so there was nothing to keep", remote)
		w.report("unshared-empty", remote, nil)
		return true
	}
	if w.ops.Detached == nil {
		w.ops.Log("vfs %s is no longer shared with you: kept in place (no parking available)", remote)
		return false
	}
	if perr := w.ops.Detached(full, server); perr != nil {
		w.ops.Log("vfs no longer shared: park %s: %v", remote, perr)
		return false
	}
	w.forget(server) // only now: a copy still inside the mount keeps its root mark for the retry
	w.ops.Log("vfs %s is no longer shared with you: kept your copy, parked", remote)
	w.report("unshared", remote, nil)
	return true
}

// forget drops everything recorded under a vanished share (no-op if unset).
func (w *Watcher) forget(remotePath string) {
	if w.ops.Forget != nil {
		w.ops.Forget(remotePath)
	}
}

// salvage turns a vanished share's local tree into plain files: hydrated
// placeholders are reverted in place (bytes kept, cloud metadata removed —
// what leaving on-demand mode does), online-only stubs are removed (they hold
// no bytes, and the server copy they point at is out of reach), directories
// last so the tree can be moved out as an ordinary folder. Reports whether
// anything with content remains. Idempotent: a plain item is left as it is.
func (w *Watcher) salvage(full string) (kept bool, err error) {
	var dirs []string
	err = filepath.WalkDir(full, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			dirs = append(dirs, p)
			return nil
		}
		fi, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		if !cfIsPlaceholder(fi, filepath.ToSlash(p)) {
			kept = true // already just bytes
			return nil
		}
		if cfIsDehydrated(fi, p) {
			if rerr := os.Remove(p); rerr != nil && !os.IsNotExist(rerr) {
				return rerr
			}
			return nil
		}
		if rerr := cfRevertPlaceholder(p); rerr != nil {
			return rerr
		}
		kept = true
		return nil
	})
	if err != nil {
		return false, err
	}
	for i := len(dirs) - 1; i >= 0; i-- { // deepest first
		p := dirs[i]
		fi, serr := os.Lstat(p)
		if serr != nil || !cfIsPlaceholder(fi, filepath.ToSlash(p)) {
			continue
		}
		if rerr := cfRevertPlaceholder(p); rerr != nil {
			return false, rerr
		}
	}
	return kept, nil
}
