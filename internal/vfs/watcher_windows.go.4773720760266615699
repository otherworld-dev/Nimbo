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
	cfInspect            = cfapi.Inspect
	cfCreatePlaceholders = cfapi.CreatePlaceholders
	cfRefreshPlaceholder = cfapi.RefreshPlaceholder
	cfRefreshIfInSync    = cfapi.RefreshPlaceholderIfInSync
	cfUpdateIdentity     = cfapi.UpdateIdentity
	cfMarkInSync         = cfapi.MarkInSync
	cfShellNotify        = cfapi.ShellNotifyUpdated
	cfSettlePin          = cfapi.SettlePin
	cfExclude            = cfapi.ExcludeFromSync
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
	// RecordFileID / FileID / DropFileID persist the server oc:fileid per remote
	// path so down-sync can recognise a server rename (old path gone, new path
	// with the same fileid) and move the placeholder instead of delete+recreate.
	RecordFileID func(remotePath, fileid string)
	FileID       func(remotePath string) (string, bool)
	DropFileID   func(remotePath string)
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
	mvAttempts  map[string]int  // consecutive MOVE failures per destination
	busyCount   map[string]int  // consecutive local-sharing-violation retries
	pokeTimer   *time.Timer     // debounced push-triggered reconcile

	loopDone chan struct{} // closed when the watch loop exits (Close joins it)

	lostEvents atomic.Bool // events were lost (overflow/reopen) — next pass sweeps

	reconMu sync.Mutex // serialises reconcile passes
	// Both guarded by reconMu (only reconcile passes touch them):
	firstPassDone bool // a full (skip-free) pass has completed since start
	fullSweep     bool // the running pass ignores the ETag subtree skip
}

const (
	uploadDebounce = 800 * time.Millisecond
	// Deletes wait a beat so a delete that's really part of a rename/atomic-save
	// (the path reappears) can be cancelled before it hits the server.
	deleteDebounce = 1200 * time.Millisecond
	// pokeDebounce coalesces a burst of notify_push events into one reconcile.
	pokeDebounce = 1500 * time.Millisecond
)

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
		loopDone: make(chan struct{}),
	}
	if w.ops.Log == nil {
		w.ops.Log = func(string, ...any) {}
	}
	go w.loop()
	if w.ops.List != nil {
		go w.pollLoop()
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
	fileActionAdded       = 0x1
	fileActionRemoved     = 0x2
	fileActionModified    = 0x3
	fileActionRenamedOld  = 0x4
	fileActionRenamedNew  = 0x5
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
			w.cancelDelete(path)
			w.scheduleUpload(path)
		case fileActionRenamedOld:
			renameOld = path
		case fileActionRenamedNew:
			if renameOld != "" {
				w.cancelDelete(renameOld)
				old := renameOld
				renameOld = ""
				go w.handleRename(old, path)
			} else {
				w.cancelDelete(path)
				w.scheduleUpload(path)
			}
		case fileActionRemoved:
			w.scheduleDelete(path)
		}
		if next == 0 {
			break
		}
		off += int(next)
	}
}

// scheduleUpload debounces handling of a path (editors fire many writes/save).
// heldRetry is how long to wait before re-checking a file somebody else has
// locked. Long enough not to hammer the server for the length of their editing
// session, short enough that the upload follows soon after they close it.
const heldRetry = 60 * time.Second

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
		w.mu.Lock()
		delete(w.delete, path)
		w.mu.Unlock()
		w.handleDelete(path)
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
		w.mu.Lock()
		delete(w.delete, path)
		w.mu.Unlock()
		w.handleDelete(path)
	})
}

func (w *Watcher) cancelDelete(path string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if t, ok := w.delete[path]; ok {
		t.Stop()
		delete(w.delete, path)
	}
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
	if !ch.NeedsUpload {
		// Nothing to upload — but the event may be a PIN change: Explorer's
		// "Free up space" sets UNPINNED and waits for US to dehydrate; until
		// then the item and its ancestors wear sync-pending arrows.
		if ok, serr := cfSettlePin(path); serr != nil {
			w.ops.Log("vfs settle pin %s: %v", path, serr)
		} else if ok {
			w.ops.Log("vfs freed up space for %s", w.remoteFor(path))
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
					w.report("upload", remote, fmt.Errorf("the file has been in use by another program for a while — Nimbo keeps retrying"))
				}
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
		w.scheduleUpload(path)
		return
	}
	if err := cfMarkInSync(path, []byte(server)); err != nil {
		w.ops.Log("vfs mark in-sync %s: %v", path, err)
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
	if w.ctx.Err() != nil {
		return
	}
	if w.isSuppressed(path) {
		return // we removed this ourselves during down-sync; already gone server-side
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
	w.mu.Lock()
	delete(w.delAttempts, key)
	delete(w.attempts, key)
	delete(w.busyCount, key)
	w.mu.Unlock()
	w.ops.Log("vfs deleted %s", server)
	w.report("delete-remote", remote, nil)
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
	src, dst := w.serverFor(oldPath, isDir), w.serverFor(newPath, isDir)
	if err := w.ops.Move(w.ctx, src, dst); err != nil {
		// 404 = the source was never on the server (a brand-new file renamed
		// before its first upload) — uploading the destination IS the answer.
		// Anything else gets smarter handling: the server may have APPLIED the
		// move and only the response was lost (falling back to upload would
		// duplicate the file and orphan the old server copy), or the failure
		// is a blip worth retrying as a MOVE.
		if transport.StatusCode(err) == 404 || strings.Contains(err.Error(), "server returned 404") {
			w.ops.Log("vfs move %s -> %s: source not on server (falling back to upload)", src, dst)
			w.handleChange(newPath)
			return
		}
		if w.ops.Stat != nil {
			if exists, serr := w.ops.Stat(dst); serr == nil && exists {
				w.ops.Log("vfs move %s -> %s: response lost but the server applied it", src, dst)
				w.finishRename(oldPath, newPath, dst)
				return
			}
		}
		if w.ctx.Err() != nil {
			return
		}
		w.ops.Log("vfs move %s -> %s: %v (will retry)", src, dst, err)
		w.report("move", w.remoteFor(newPath), err)
		w.mu.Lock()
		w.mvAttempts[strings.ToLower(newPath)]++
		n := w.mvAttempts[strings.ToLower(newPath)]
		w.mu.Unlock()
		if n <= renameMaxRetries {
			old, np := oldPath, newPath
			time.AfterFunc(retryDelay(n), func() { w.handleRename(old, np) })
		} else {
			w.ops.Log("vfs move %s -> %s: giving up after %d attempts", src, dst, n)
		}
		return
	}
	w.finishRename(oldPath, newPath, dst)
}

// renameMaxRetries bounds MOVE retries — beyond it the rename is reported and
// left for reconcile to sort out rather than retried forever.
const renameMaxRetries = 8

// finishRename records a rename the server now agrees with.
func (w *Watcher) finishRename(oldPath, newPath, dst string) {
	w.mu.Lock()
	delete(w.mvAttempts, strings.ToLower(newPath))
	w.mu.Unlock()
	w.ops.Log("vfs moved %s -> %s", w.serverFor(oldPath, false), dst)
	w.report("move", w.remoteFor(newPath), nil)
	// The identity must name the file as the SERVER knows it, or hydration 404s.
	if err := cfUpdateIdentity(newPath, []byte(dst)); err != nil {
		w.ops.Log("vfs repoint identity %s: %v", newPath, err)
	}
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
		return false // can't read locally — don't claim reconciled
	}
	if len(entries) == 0 {
		return true // unpopulated (lazy) — nothing to do
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
			ch, ierr := cfInspect(full)
			if ierr != nil || ch.NeedsUpload {
				if ierr == nil {
					w.scheduleUploadIfIdle(full)
				}
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
											for _, k := range remoteKids {
												w.recordBaseline(string(k.Identity), k.ETag)
												if !k.IsDir {
													w.recordFileID(string(k.Identity), k.FileID)
												}
											}
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
		if ch, ierr := cfInspect(full); ierr == nil && ch.NeedsUpload {
			w.scheduleUploadIfIdle(full)
			continue
		}
		// File present both sides: if the server copy changed and our copy is
		// in-sync (clean), refresh it so a previously-downloaded file isn't stale.
		// (A dirty local copy is a pending upload / potential conflict — left alone.)
		if w.remoteChanged(r, fi, string(r.Identity)) {
			if ch, ierr := cfInspect(full); ierr == nil && !ch.NeedsUpload {
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
			for _, r := range toCreate {
				w.recordBaseline(string(r.Identity), r.ETag)
				if !r.IsDir {
					w.recordFileID(string(r.Identity), r.FileID) // enable rename detection later
				}
				w.report("download", string(r.Identity), nil) // surface new server files in the activity feed
			}
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
