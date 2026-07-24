//go:build windows

// Package vfs provides write-back for on-demand (Cloud Files) folders: it
// watches a mounted sync root for user changes and pushes them to the server
// (uploads, folder creates, deletes, renames), then marks placeholders in-sync
// so its own writes aren't re-processed. This is separate from the two-way diff
// engine, which must never scan placeholder folders.
package vfs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/otherworld/nimbo/internal/cfapi"
)

// cfapi seams — package-level function vars so tests can substitute an
// in-memory placeholder world for the live Cloud Files API. Production code
// always uses the real implementations.
var (
	cfInspect            = cfapi.Inspect
	cfCreatePlaceholders = cfapi.CreatePlaceholders
	cfRefreshPlaceholder = cfapi.RefreshPlaceholder
	cfUpdateIdentity     = cfapi.UpdateIdentity
	cfMarkInSync         = cfapi.MarkInSync
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
	Log          func(format string, args ...any)
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

	mu        sync.Mutex
	upload    map[string]*time.Timer // debounced uploads (coalesce write bursts)
	delete    map[string]*time.Timer // debounced deletes (cancelled if path returns)
	suppress  map[string]time.Time   // paths we removed ourselves (skip server delete)
	pokeTimer *time.Timer            // debounced push-triggered reconcile
	reconMu   sync.Mutex             // serialises reconcile passes
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

// Close stops the watcher.
func (w *Watcher) Close() {
	w.cancel()
	windows.CancelIoEx(w.handle, nil)
	windows.CloseHandle(w.handle)
}

// FILE_NOTIFY_INFORMATION action codes.
const (
	fileActionAdded      = 0x1
	fileActionRemoved    = 0x2
	fileActionModified   = 0x3
	fileActionRenamedOld = 0x4
	fileActionRenamedNew = 0x5
	fileNotifyChangeFlags = windows.FILE_NOTIFY_CHANGE_FILE_NAME |
		windows.FILE_NOTIFY_CHANGE_DIR_NAME |
		windows.FILE_NOTIFY_CHANGE_SIZE |
		windows.FILE_NOTIFY_CHANGE_LAST_WRITE
)

func (w *Watcher) loop() {
	buf := make([]byte, 64*1024)
	for {
		var n uint32
		err := windows.ReadDirectoryChanges(w.handle, &buf[0], uint32(len(buf)),
			true, // watch the whole subtree — covers lazily-populated subdirs
			fileNotifyChangeFlags, &n, nil, 0)
		if err != nil || w.ctx.Err() != nil {
			return // handle closed (Close) or error → stop
		}
		w.parse(buf[:n])
	}
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
	switch {
	case strings.HasSuffix(base, ".nimbo-part"): // engine download temp
		return true
	case base == "Thumbs.db" || base == "desktop.ini" || base == ".DS_Store":
		return true
	case strings.HasPrefix(base, "~$") || strings.HasPrefix(base, ".~lock."):
		return true
	case strings.HasSuffix(base, ".tmp") || strings.HasSuffix(base, ".~tmp") || strings.HasSuffix(base, ".crdownload"):
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

// handleChange uploads one changed path if it's a user create/modify, then marks
// it in-sync so the next notification (from our own write) is ignored.
func (w *Watcher) handleChange(path string) {
	if w.ctx.Err() != nil {
		return
	}
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
		return
	}
	remote := w.remoteFor(path)
	if remote == "" {
		return
	}
	if ch.IsDir {
		if err := w.ops.Mkdir(w.ctx, remote); err != nil {
			w.ops.Log("vfs mkdir %s: %v", remote, err)
			w.report("mkdir-remote", remote, err)
			return
		}
		w.report("mkdir-remote", remote, nil)
	} else {
		if err := w.ops.Upload(w.ctx, path, remote); err != nil {
			w.ops.Log("vfs upload %s -> %s: %v", path, remote, err)
			w.report("upload", remote, err)
			return
		}
		w.ops.Log("vfs uploaded %s (%d bytes)", remote, info.Size())
		w.report("upload", remote, nil)
	}
	if err := cfMarkInSync(path, []byte(remote)); err != nil {
		w.ops.Log("vfs mark in-sync %s: %v", path, err)
	}
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
	if _, err := os.Lstat(path); err == nil {
		return // came back — not a real delete
	}
	remote := w.remoteFor(path)
	if remote == "" {
		return // never delete the sync root
	}
	if err := w.ops.Delete(w.ctx, remote); err != nil {
		w.ops.Log("vfs delete %s: %v", remote, err)
		w.report("delete-remote", remote, err)
		return
	}
	w.ops.Log("vfs deleted %s", remote)
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
	src, dst := w.remoteFor(oldPath), w.remoteFor(newPath)
	if src == "" || dst == "" {
		return
	}
	if err := w.ops.Move(w.ctx, src, dst); err != nil {
		w.ops.Log("vfs move %s -> %s: %v (falling back to upload)", src, dst, err)
		w.handleChange(newPath)
		return
	}
	w.ops.Log("vfs moved %s -> %s", src, dst)
	w.report("move", dst, nil)
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
	w.reconcileDir("", "")
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
	if knownETag != "" {
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
	remoteByName := make(map[string]cfapi.PlaceholderInfo, len(remote))
	for _, r := range remote {
		remoteByName[r.Name] = r
	}

	// Pre-pass: which names exist locally, and which remote files are additions
	// (no local placeholder by that name) keyed by oc:fileid — the candidates a
	// server rename could have produced.
	localByName := map[string]bool{}
	for _, e := range entries {
		if !skipName(e.Name()) {
			localByName[e.Name()] = true
		}
	}
	addByFileID := map[string]cfapi.PlaceholderInfo{}
	for _, r := range remote {
		if !r.IsDir && r.FileID != "" && !localByName[r.Name] {
			addByFileID[r.FileID] = r
		}
	}
	consumed := map[string]bool{} // remote additions claimed as a rename target

	type subdir struct{ rel, etag string }
	var subdirs []subdir
	for _, e := range entries {
		name := e.Name()
		if skipName(name) {
			continue
		}
		full := filepath.Join(localDir, name)
		r, inRemote := remoteByName[name]
		if !inRemote {
			// Local has it, server doesn't. Only touch an in-sync placeholder
			// (known to mirror the server); a not-in-sync item is a pending
			// upload / brand-new local file and must be kept.
			ch, ierr := cfInspect(full)
			if ierr != nil || ch.NeedsUpload {
				continue
			}
			// Rename detection: if this gone-from-server file's recorded fileid
			// reappears as a server-side addition, the server renamed it. Move the
			// local placeholder (preserving any hydration) instead of delete+create.
			if !ch.IsDir {
				if fid, ok := w.fileIDFor(w.remoteFor(full)); ok {
					if y, isRename := addByFileID[fid]; isRename && !consumed[y.Name] {
						if w.pullRename(localDir, full, y, fid) {
							consumed[y.Name] = true
							continue
						}
					}
				}
			}
			// Not a rename — propagate the server-side delete locally.
			w.suppressDelete(full)
			if rerr := os.RemoveAll(full); rerr != nil {
				w.ops.Log("vfs reconcile remove %s: %v", name, rerr)
			} else {
				w.ops.Log("vfs pulled delete %s", w.remoteFor(full))
				w.report("delete-local", w.remoteFor(full), nil)
				w.dropFileID(w.remoteFor(full))
			}
			continue
		}
		if e.IsDir() {
			child := name
			if rel != "" {
				child = rel + "/" + name
			}
			subdirs = append(subdirs, subdir{rel: child, etag: r.ETag})
			continue
		}
		// File present both sides: if the server copy changed and our copy is
		// in-sync (clean), refresh it so a previously-downloaded file isn't stale.
		// (A dirty local copy is a pending upload / potential conflict — left alone.)
		if fi, statErr := e.Info(); statErr == nil && w.remoteChanged(r, fi, string(r.Identity)) {
			if ch, ierr := cfInspect(full); ierr == nil && !ch.NeedsUpload {
				if rerr := cfRefreshPlaceholder(full, r.Identity, r.Size, r.ModTime); rerr != nil {
					w.ops.Log("vfs refresh %s: %v", name, rerr)
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
	var toCreate []cfapi.PlaceholderInfo
	for _, r := range remote {
		if !localByName[r.Name] && !consumed[r.Name] {
			toCreate = append(toCreate, r)
		}
	}
	if len(toCreate) > 0 {
		if cerr := cfCreatePlaceholders(localDir, toCreate); cerr != nil {
			w.ops.Log("vfs reconcile create in %q: %v", rel, cerr)
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

	ok := true
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
