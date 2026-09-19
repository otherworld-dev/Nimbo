//go:build windows

package vfs

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"golang.org/x/sys/windows"

	"github.com/otherworld/nimbo/internal/cfapi"
)

// --- test fakes -------------------------------------------------------------

// fakeCf swaps the cfapi seams for an in-memory placeholder world: every path
// is an in-sync placeholder unless listed in dirty (NeedsUpload). Created
// placeholders become real (empty) files/dirs so os.ReadDir sees them.
type fakeCf struct {
	mu    sync.Mutex
	dirty map[string]bool // path -> NeedsUpload (the in-sync bit is clear)
	// modified is the OTHER half of "dirty": paths holding local content the
	// server has not got (CF_PLACEHOLDER_STANDARD_INFO.ModifiedDataSize > 0).
	// The live filter clears the in-sync bit on any rename/move as well as on
	// an edit, so dirty without modified is exactly what a clean placeholder
	// looks like the instant after it is moved.
	modified      map[string]bool
	refreshed     []string          // paths passed to RefreshPlaceholder
	repointed     []string          // paths passed to UpdateIdentity (MARK_IN_SYNC)
	repointedKeep []string          // paths passed to UpdateIdentityKeepState
	marked        []string          // paths passed to MarkInSync
	inSynced      []string          // paths passed to SetInSync (the in-sync heal)
	identities    map[string]string // path -> identity stamped by MarkInSync
	created       []string          // names passed to CreatePlaceholders
	plain         map[string]bool   // paths that are NOT placeholders (flattened)
	corrupt       map[string]bool   // paths Windows refuses to open (ERROR_CLOUD_FILE_METADATA_CORRUPT)
	// populated holds directory placeholders whose FETCH_PLACEHOLDERS transfer
	// has completed (the driver clears RECALL_ON_DATA_ACCESS and never asks
	// again). Default false, like a freshly created lazy directory; a PLAIN
	// directory is always populated and is answered from plain, not here.
	populated     map[string]bool
	notified      []string         // paths passed to the shell change-notify seam
	settleChecked []string         // paths offered to the pin-settle seam
	excluded      []string         // paths passed to the exclude-from-sync seam
	reverted      []string         // paths passed to RevertPlaceholder (they become plain)
	dehydrated    map[string]bool  // paths that are online-only stubs (no bytes)
	pinned        map[string]bool  // paths that read as pinned, online-only placeholders
	hydrated      []string         // paths passed to the hydrate-if-pinned seam
	hydrateGate   chan struct{}    // when non-nil, hydration blocks until it closes
	hydrateCalls  map[string]int   // per-path invocation count of the hydrate-if-pinned seam
	hydrateErr    map[string]error // path -> error the hydrate-if-pinned seam returns
	hydrateDone   int              // downloads that ran all the way past the gate
	hydrateFlight int              // downloads running right now
	hydratePeak   int              // the most that ever ran at once
	events        []string         // ordered "refresh:<path>" / "hydrate:<path>" log
}

// createdNames returns the names reconcile asked to be created. Asserting on
// this rather than on the filesystem matters: NTFS is case-insensitive, so
// os.Stat("photos") succeeds when "Photos" exists and a filesystem check would
// silently pass for the wrong reason.
func (f *fakeCf) createdNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.created...)
}

func installFakeCf(t *testing.T) *fakeCf {
	t.Helper()
	f := &fakeCf{dirty: map[string]bool{}, modified: map[string]bool{}, identities: map[string]string{}, plain: map[string]bool{}, corrupt: map[string]bool{}, populated: map[string]bool{}, dehydrated: map[string]bool{}, pinned: map[string]bool{}, hydrateCalls: map[string]int{}, hydrateErr: map[string]error{}}
	oi, oc, or, ou, om, op, on, os2, ox := cfInspect, cfCreatePlaceholders, cfRefreshPlaceholder, cfUpdateIdentity, cfMarkInSync, cfIsPlaceholder, cfShellNotify, cfSettlePin, cfExclude
	ov, orv, od := cfRefreshIfInSync, cfRevertPlaceholder, cfIsDehydrated
	opi := cfPlaceholderIdentity
	opm := cfPlaceholderModified
	ouk := cfUpdateIdentityKeep
	opd, ohp := cfPinnedDehydrated, cfHydrateIfPinned
	osi := cfSetInSync
	odp := cfDirPopulated
	t.Cleanup(func() {
		cfSetInSync = osi
		cfDirPopulated = odp
		cfInspect, cfCreatePlaceholders, cfRefreshPlaceholder, cfUpdateIdentity, cfMarkInSync, cfIsPlaceholder, cfShellNotify, cfSettlePin, cfExclude = oi, oc, or, ou, om, op, on, os2, ox
		cfRefreshIfInSync, cfRevertPlaceholder, cfIsDehydrated = ov, orv, od
		cfPlaceholderIdentity = opi
		cfPlaceholderModified = opm
		cfUpdateIdentityKeep = ouk
		cfPinnedDehydrated, cfHydrateIfPinned = opd, ohp
	})
	cfDirPopulated = func(path string) (bool, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		key := strings.ToLower(filepath.ToSlash(path))
		if f.plain[key] {
			return true, nil // a plain directory holds everything it has
		}
		return f.populated[key], nil
	}
	cfPlaceholderIdentity = func(path string) ([]byte, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.corrupt[strings.ToLower(path)] {
			// The real reader OPENS the file; Windows refuses with 363. (The
			// listing-based inspect never opens it and reads it as healthy.)
			return nil, &os.PathError{Op: "CreateFile", Path: path, Err: windows.ERROR_CLOUD_FILE_METADATA_CORRUPT}
		}
		if f.plain[strings.ToLower(filepath.ToSlash(path))] {
			return nil, cfapi.ErrNotPlaceholder
		}
		id, ok := f.identities[strings.ToLower(path)]
		if !ok {
			return nil, cfapi.ErrNotPlaceholder
		}
		return []byte(id), nil
	}
	cfPlaceholderModified = func(path string) (bool, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		// A plain file is never-uploaded content, exactly as the real one
		// reports it (ErrNotPlaceholder -> modified).
		return f.plain[strings.ToLower(filepath.ToSlash(path))] || f.modified[strings.ToLower(path)], nil
	}
	cfRevertPlaceholder = func(path string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.reverted = append(f.reverted, path)
		f.plain[strings.ToLower(filepath.ToSlash(path))] = true // a reverted placeholder is a plain item
		return nil
	}
	cfIsDehydrated = func(_ os.FileInfo, full string) bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.dehydrated[strings.ToLower(filepath.ToSlash(full))]
	}
	cfRefreshIfInSync = func(path string, identity []byte, size int64, mtime time.Time) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.dirty[strings.ToLower(path)] {
			return cfapi.ErrNotInSync // verify flag: a dirty placeholder refuses the refresh
		}
		f.refreshed = append(f.refreshed, path)
		f.events = append(f.events, "refresh:"+path)
		return nil
	}
	cfExclude = func(path string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.excluded = append(f.excluded, path)
		return nil
	}
	cfSettlePin = func(path string) (bool, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.settleChecked = append(f.settleChecked, path)
		return false, nil
	}
	cfShellNotify = func(path string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.notified = append(f.notified, path)
	}
	cfIsPlaceholder = func(_ os.FileInfo, full string) bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return !f.plain[strings.ToLower(filepath.ToSlash(full))]
	}
	cfInspect = func(path string) (cfapi.Change, error) {
		fi, err := os.Lstat(path)
		if err != nil {
			return cfapi.Change{}, err
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		// Mirrors the real Inspect: the in-sync bit is a PRE-FILTER, not the
		// verdict. A plain item always needs uploading (never uploaded); a
		// placeholder that is in sync never does; one that is NOT in sync
		// needs uploading only if it holds unsynced local content — which is
		// exactly what tells a real edit from a bit the filter cleared on a
		// rename.
		plain := f.plain[strings.ToLower(filepath.ToSlash(path))]
		key := strings.ToLower(path)
		ch := cfapi.Change{IsDir: fi.IsDir(), Placeholder: !plain, InSync: !plain && !f.dirty[key]}
		switch {
		case fi.IsDir():
			ch.NeedsUpload = plain
		case plain:
			ch.NeedsUpload = true
		case ch.InSync:
			ch.NeedsUpload = false
		default:
			ch.NeedsUpload = f.modified[key]
		}
		return ch, nil
	}
	cfSetInSync = func(path string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.plain[strings.ToLower(filepath.ToSlash(path))] {
			return cfapi.ErrNotPlaceholder
		}
		f.inSynced = append(f.inSynced, path)
		delete(f.dirty, strings.ToLower(path)) // the bit is back
		return nil
	}
	cfCreatePlaceholders = func(baseDir string, items []cfapi.PlaceholderInfo) error {
		f.mu.Lock()
		for _, it := range items {
			f.created = append(f.created, it.Name)
			// What it says on the tin: whatever was at that path, there is a
			// PLACEHOLDER there now (and a directory placeholder starts out
			// lazy - nothing has populated it yet).
			delete(f.plain, strings.ToLower(filepath.ToSlash(filepath.Join(baseDir, it.Name))))
		}
		f.mu.Unlock()
		for _, it := range items {
			p := filepath.Join(baseDir, it.Name)
			if it.IsDir {
				if err := os.MkdirAll(p, 0o755); err != nil {
					return err
				}
			} else if err := os.WriteFile(p, nil, 0o644); err != nil {
				return err
			}
		}
		return nil
	}
	cfRefreshPlaceholder = func(path string, identity []byte, size int64, mtime time.Time) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.refreshed = append(f.refreshed, path)
		return nil
	}
	cfUpdateIdentity = func(path string, identity []byte) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.repointed = append(f.repointed, path)
		f.identities[strings.ToLower(path)] = string(identity)
		// MARK_IN_SYNC: the placeholder now claims to mirror the server, so
		// whatever was waiting to upload is no longer waiting.
		delete(f.dirty, strings.ToLower(path))
		return nil
	}
	cfUpdateIdentityKeep = func(path string, identity []byte) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.repointedKeep = append(f.repointedKeep, path)
		f.identities[strings.ToLower(path)] = string(identity)
		return nil // the in-sync state, dirty bit and all, is left alone
	}
	cfMarkInSync = func(path string, identity []byte) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.marked = append(f.marked, path)
		// The real one only sets the in-sync BIT on something that is already
		// a placeholder (CfSetInSyncState); it stamps an identity only when it
		// has to CONVERT a plain item. A placeholder whose identity names
		// another server path therefore KEEPS that identity across an upload
		// — which is why an upload can leave a file in sync and pointing at a
		// server path that no longer exists.
		if _, ok := f.identities[strings.ToLower(path)]; !ok {
			f.identities[strings.ToLower(path)] = string(identity)
		}
		return nil
	}
	cfPinnedDehydrated = func(path string) bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.pinned[strings.ToLower(path)]
	}
	cfHydrateIfPinned = func(path string) (bool, error) {
		key := strings.ToLower(path)
		f.mu.Lock()
		f.hydrateCalls[key]++
		if err, ok := f.hydrateErr[key]; ok {
			f.mu.Unlock()
			return false, err
		}
		if !f.pinned[key] {
			f.mu.Unlock()
			return false, nil
		}
		delete(f.pinned, key)
		f.hydrated = append(f.hydrated, path)
		f.events = append(f.events, "hydrate:"+path)
		f.hydrateFlight++
		if f.hydrateFlight > f.hydratePeak {
			f.hydratePeak = f.hydrateFlight
		}
		gate := f.hydrateGate
		f.mu.Unlock()
		if gate != nil {
			<-gate
		}
		f.mu.Lock()
		f.hydrateFlight--
		f.hydrateDone++
		f.mu.Unlock()
		return true, nil
	}
	return f
}

// identityOf returns the identity stamped on a path by MarkInSync. The identity
// is what the OS hands back on hydration, so for a disguised file it must name
// the file as the SERVER knows it — a detail no test could observe before.
func (f *fakeCf) identityOf(path string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.identities[strings.ToLower(path)]
	return id, ok
}

// markPlain makes a path read as a plain non-placeholder file (the #580
// flattening aftermath).
// markCorrupt makes every OPEN of path (the identity read) fail the way
// Windows fails an entry whose cloud-file metadata is corrupt (error 363),
// while the listing-based inspect still reads it as a healthy in-sync
// placeholder — exactly the real shape.
func (f *fakeCf) markCorrupt(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.corrupt[strings.ToLower(path)] = true
}

func (f *fakeCf) markPlain(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plain[strings.ToLower(filepath.ToSlash(path))] = true
}

// setIdentity stamps the identity a placeholder carries (what the server
// calls it), without going through MarkInSync.
func (f *fakeCf) setIdentity(path, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.identities[strings.ToLower(path)] = id
}

// markPopulated makes a directory placeholder read as already populated - its
// FETCH_PLACEHOLDERS transfer completed, so the shell will never ask for it
// again, whether it delivered entries or not.
func (f *fakeCf) markPopulated(dir string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.populated[strings.ToLower(filepath.ToSlash(dir))] = true
}

// markDehydrated makes a path read as an online-only stub: a placeholder with
// no bytes on disk.
func (f *fakeCf) markDehydrated(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dehydrated[strings.ToLower(filepath.ToSlash(path))] = true
}

func (f *fakeCf) revertedPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reverted...)
}

// isDirty reports whether the fake still has a pending upload for path.
func (f *fakeCf) isDirty(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dirty[strings.ToLower(path)]
}

// keepStateRepoints returns the paths repointed WITHOUT MARK_IN_SYNC.
func (f *fakeCf) keepStateRepoints() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.repointedKeep...)
}

// markInSyncRepoints returns the paths repointed WITH MARK_IN_SYNC — the
// branch that also restores the in-sync bit the move cleared.
func (f *fakeCf) markInSyncRepoints() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.repointed...)
}

// markDirty puts a path in the state a LOCALLY EDITED file is really in: the
// in-sync bit cleared AND unsynced local content behind it. Use markNotInSync
// for the other way the bit goes — a rename or move, which clears it while
// leaving the content exactly as the server has it.
func (f *fakeCf) markDirty(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dirty[strings.ToLower(path)] = true
	f.modified[strings.ToLower(path)] = true
}

// markNotInSync clears ONLY the in-sync bit, which is what the cloud filter
// does to any placeholder another process renames or moves (measured live
// 2026-09-15). The file still holds exactly what the server has.
func (f *fakeCf) markNotInSync(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dirty[strings.ToLower(path)] = true
}

// markModified gives a path unsynced local content — a real edit, not merely
// an in-sync bit the filter cleared. Callers that want the state a locally
// edited file is actually in mark it dirty as well.
func (f *fakeCf) markModified(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.modified[strings.ToLower(path)] = true
}

// refreshedPaths returns the paths whose placeholder was refreshed from the
// server listing.
func (f *fakeCf) refreshedPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.refreshed...)
}

// inSyncedPaths returns the paths whose in-sync state was restored by the heal.
func (f *fakeCf) inSyncedPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.inSynced...)
}

// markPinned makes a path read as a pinned, online-only placeholder.
func (f *fakeCf) markPinned(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pinned[strings.ToLower(path)] = true
}

// hydrateCompleted counts the downloads that ran to completion — hydratedPaths
// records a path as the seam STARTS it, gate and all.
func (f *fakeCf) hydrateCompleted() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hydrateDone
}

// hydratePeakInFlight reports the most downloads the seam ever had running at
// the same time.
func (f *fakeCf) hydratePeakInFlight() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hydratePeak
}

func (f *fakeCf) hydratedPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.hydrated...)
}

// hydrateCallCount returns how many times the hydrate-if-pinned seam was
// invoked for path, regardless of outcome.
func (f *fakeCf) hydrateCallCount(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hydrateCalls[strings.ToLower(path)]
}

// markHydrateErr makes the hydrate-if-pinned seam fail for path with err.
func (f *fakeCf) markHydrateErr(path string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hydrateErr[strings.ToLower(path)] = err
}

// eventIndex returns the position of the first occurrence of event ("refresh:
// <path>" / "hydrate:<path>") in the ordered log, or -1 if absent.
func (f *fakeCf) eventIndex(event string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, e := range f.events {
		if e == event {
			return i
		}
	}
	return -1
}

// recorder collects the server-side ops the watcher performs.
type recorder struct {
	mu        sync.Mutex
	uploads   []string
	mkdirs    []string
	deletes   []string
	moves     [][2]string
	baselines map[string]string
	fileids   map[string]string
	listCalls map[string]int
	listing   map[string][]cfapi.PlaceholderInfo
	listErr   error
	uploaded  chan string
	deleted   chan string
	moved     chan [2]string

	uploadFails int           // fail this many uploads before succeeding…
	uploadErr   error         // …with this error
	uploadGate  chan struct{} // when non-nil, Upload blocks until it closes
	deleteFails int
	deleteErr   error
	mkdirFails  int
	mkdirErr    error
	moveFails   int
	moveErr     error
	moveGate    chan struct{}   // when non-nil, Move blocks until it closes
	statExists  map[string]bool // remote -> exists (nil map = Stat unavailable)
	cancelled   []string        // uploads whose ctx was cancelled mid-flight
	reports     []reportRec     // Report calls (kind, path, err)
	mountRoots  map[string]bool // remote paths the listing marked as share/mount roots (nil = hook absent)
	detached    [][2]string     // Ops.Detached calls (local path, remote path)
	detachedErr error           // what Ops.Detached returns
	forgotten   []string        // Ops.Forget calls (remote path)

	// The real baseline store persists its WHOLE file on every call, so the
	// SHAPE of the writes is the thing under test for a subtree: these count
	// the calls, not just the resulting map.
	moveBatches   [][][2]string       // Ops.MoveBaselines calls
	recordBatches []map[string]string // Ops.RecordBaselines calls
	singleRecords int                 // Ops.RecordBaseline (one-item) calls
	singleForgets int                 // Ops.ForgetBaseline (one-item) calls
	uploadSawBase map[string]string   // the baseline each Upload saw at call time

	logs []string // Ops.Log lines (formatted)
}

// logLines returns the log lines the watcher has emitted so far.
func (r *recorder) logLines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.logs...)
}

// logged reports whether any log line contains sub.
func (r *recorder) logged(sub string) bool {
	for _, l := range r.logLines() {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

// baselineBatches returns the batched baseline-carry calls made so far.
func (r *recorder) baselineBatches() [][][2]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][][2]string(nil), r.moveBatches...)
}

// forgottenPaths returns the remote paths the watcher asked to be forgotten.
func (r *recorder) forgottenPaths() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.forgotten...)
}

// detachedCalls returns the folders handed over for parking so far.
func (r *recorder) detachedCalls() [][2]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][2]string(nil), r.detached...)
}

type reportRec struct {
	kind, path string
	err        error
}

func newRecorder() *recorder {
	return &recorder{
		baselines: map[string]string{}, fileids: map[string]string{},
		listCalls: map[string]int{}, listing: map[string][]cfapi.PlaceholderInfo{},
		uploadSawBase: map[string]string{},
		uploaded:      make(chan string, 16), deleted: make(chan string, 16), moved: make(chan [2]string, 16),
	}
}

func (r *recorder) ops() Ops {
	var mountRoot func(string) bool
	var detached func(string, string) error
	var forget func(string)
	if r.mountRoots != nil {
		forget = func(remote string) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.forgotten = append(r.forgotten, remote)
		}
		mountRoot = func(remote string) bool {
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.mountRoots[remote]
		}
		detached = func(local, remote string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.detachedErr != nil {
				return r.detachedErr
			}
			r.detached = append(r.detached, [2]string{local, remote})
			return nil
		}
	}
	return Ops{
		MountRoot: mountRoot,
		Detached:  detached,
		Forget:    forget,
		Upload: func(ctx context.Context, local, remote string) error {
			r.mu.Lock()
			gate := r.uploadGate
			if r.uploadFails > 0 {
				r.uploadFails--
				err := r.uploadErr
				r.mu.Unlock()
				return err
			}
			r.uploads = append(r.uploads, remote)
			// What the conflict check will read: the baseline recorded for
			// this remote path at the moment the upload actually runs.
			r.uploadSawBase[remote] = r.baselines[remote]
			r.mu.Unlock()
			if gate != nil {
				select {
				case <-gate:
				case <-ctx.Done():
					r.mu.Lock()
					r.cancelled = append(r.cancelled, remote)
					r.mu.Unlock()
					return ctx.Err()
				}
			}
			r.uploaded <- remote
			return nil
		},
		Stat: func(remote string) (bool, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.statExists == nil {
				return false, nil
			}
			return r.statExists[remote], nil
		},
		Report: func(kind, path string, err error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.reports = append(r.reports, reportRec{kind, path, err})
		},
		Mkdir: func(_ context.Context, remote string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.mkdirFails > 0 {
				r.mkdirFails--
				return r.mkdirErr
			}
			r.mkdirs = append(r.mkdirs, remote)
			return nil
		},
		Delete: func(_ context.Context, remote string) error {
			r.mu.Lock()
			if r.deleteFails > 0 {
				r.deleteFails--
				err := r.deleteErr
				r.mu.Unlock()
				return err
			}
			r.deletes = append(r.deletes, remote)
			r.mu.Unlock()
			r.deleted <- remote
			return nil
		},
		Move: func(_ context.Context, src, dst string) error {
			r.mu.Lock()
			if r.moveFails > 0 {
				r.moveFails--
				err := r.moveErr
				r.mu.Unlock()
				return err
			}
			gate := r.moveGate
			r.mu.Unlock()
			if gate != nil {
				<-gate // hold the MOVE in flight
			}
			r.mu.Lock()
			r.moves = append(r.moves, [2]string{src, dst})
			r.mu.Unlock()
			r.moved <- [2]string{src, dst}
			return nil
		},
		List: func(rel string) ([]cfapi.PlaceholderInfo, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.listCalls[rel]++
			if r.listErr != nil {
				return nil, r.listErr
			}
			return r.listing[rel], nil
		},
		RecordBaseline: func(remote, etag string) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.singleRecords++
			r.baselines[remote] = etag
		},
		Baseline: func(remote string) (string, bool) {
			r.mu.Lock()
			defer r.mu.Unlock()
			e, ok := r.baselines[remote]
			return e, ok
		},
		ForgetBaseline: func(remote string) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.singleForgets++
			delete(r.baselines, remote)
		},
		// The batch forms, as the real store implements them: one persist per
		// call, whatever the number of paths.
		MoveBaselines: func(pairs [][2]string) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.moveBatches = append(r.moveBatches, append([][2]string(nil), pairs...))
			for _, p := range pairs {
				if e, ok := r.baselines[p[0]]; ok {
					if e != "" {
						r.baselines[p[1]] = e
					}
					delete(r.baselines, p[0])
				}
			}
		},
		RecordBaselines: func(m map[string]string) {
			r.mu.Lock()
			defer r.mu.Unlock()
			cp := make(map[string]string, len(m))
			for k, v := range m {
				cp[k] = v
				if v != "" {
					r.baselines[k] = v
				}
			}
			r.recordBatches = append(r.recordBatches, cp)
		},
		Log: func(format string, args ...any) {
			line := fmt.Sprintf(format, args...)
			r.mu.Lock()
			defer r.mu.Unlock()
			r.logs = append(r.logs, line)
		},
		RecordFileID: func(remote, id string) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.fileids[remote] = id
		},
		FileID: func(remote string) (string, bool) {
			r.mu.Lock()
			defer r.mu.Unlock()
			id, ok := r.fileids[remote]
			return id, ok
		},
		DropFileID: func(remote string) {
			r.mu.Lock()
			defer r.mu.Unlock()
			delete(r.fileids, remote)
		},
		// Same contract as the real store (etagStore.knownBeneath): a
		// baseline strictly beneath, never the path's own.
		KnownBeneath: func(remote string) bool {
			r.mu.Lock()
			defer r.mu.Unlock()
			prefix := strings.Trim(remote, "/") + "/"
			for k := range r.baselines {
				if strings.HasPrefix(k, prefix) {
					return true
				}
			}
			return false
		},
	}
}

// bareWatcher builds a Watcher for direct method tests (no OS watch loop).
func bareWatcher(root string, ops Ops) *Watcher {
	ctx, cancel := context.WithCancel(context.Background())
	w := &Watcher{
		root: root, remoteRoot: "", ops: ops, ctx: ctx, cancel: cancel,
		upload: map[string]*time.Timer{}, delete: map[string]*time.Timer{},
		suppress: map[string]time.Time{},
		inflight: map[string]context.CancelFunc{}, again: map[string]bool{},
		attempts: map[string]int{}, delAttempts: map[string]int{},
		mvAttempts: map[string]int{}, busyCount: map[string]int{},
		moved:       map[string]time.Time{},
		inMove:      map[string]bool{},
		verifying:   map[string]bool{},
		forced:      map[string]bool{},
		hydrateWake: make(chan struct{}, 1), hydrating: map[string]bool{},
		hydrateFails: map[string]int{}, hydrateNext: map[string]time.Time{},
	}
	if w.ops.Log == nil {
		w.ops.Log = func(string, ...any) {}
	}
	for i := 0; i < hydrateWorkers; i++ {
		go w.hydrateLoop() // New starts these too; they exit with w.cancel()
	}
	return w
}

func ph(name string, dir bool, etag, fileid string) cfapi.PlaceholderInfo {
	return cfapi.PlaceholderInfo{
		Name: name, IsDir: dir, ModTime: time.Now(), Size: 0,
		Identity: []byte(name), ETag: etag, FileID: fileid,
	}
}

// --- pure helpers -----------------------------------------------------------

func TestSkipName(t *testing.T) {
	skip := []string{
		`doc.nimbo-part`, `sub\doc.nimbo-part`, `Thumbs.db`, `desktop.ini`, `.DS_Store`,
		`~$report.docx`, `.~lock.report.odt#`, `save.tmp`, `dl.crdownload`, `x.~tmp`,
		// Windows writes these with varying case — the comparison must not care
		// (live bug: "Desktop.ini" was adopted and conflict-copied to the server).
		`Desktop.ini`, `THUMBS.DB`, `SAVE.TMP`,
		// The official Nextcloud/ownCloud client's local artifacts: its sync DB
		// and log live inside the sync folder and are left behind on migration —
		// pure pollution, never sync them.
		`.sync_74b7ab355ec5.db`, `.sync_74b7ab355ec5.db-wal`, `.sync_74b7ab355ec5.db-shm`,
		`.sync_abc.db-journal`, `.nextcloudsync.log`, `.owncloudsync.log`,
		`sub\.sync_11ff.db`, `.Nextcloudsync.log`,
	}
	keep := []string{`doc.txt`, `sub\doc.txt`, `tmp`, `partial.part2`, `lock.txt`, `a~$b`,
		// Not artifacts: user files that merely resemble them.
		`sync.db`, `my.sync_notes.txt`, `.syncthing.txt`, `desktop.initial.png`}
	for _, n := range skip {
		if !skipName(n) {
			t.Errorf("skipName(%q) = false, want true", n)
		}
	}
	for _, n := range keep {
		if skipName(n) {
			t.Errorf("skipName(%q) = true, want false", n)
		}
	}
}

func TestRemoteFor(t *testing.T) {
	w := bareWatcher(`C:\root`, Ops{})
	w.remoteRoot = "Photos"
	if got := w.remoteFor(`C:\root\sub\a.txt`); got != "Photos/sub/a.txt" {
		t.Errorf("remoteFor nested = %q", got)
	}
	if got := w.remoteFor(`C:\root`); got != "" {
		t.Errorf("remoteFor(root) = %q, want \"\" (must never delete the root)", got)
	}
	w.remoteRoot = ""
	if got := w.remoteFor(`C:\root\a.txt`); got != "a.txt" {
		t.Errorf("remoteFor account-root = %q", got)
	}
}

func TestServerChanged(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	if !serverChanged(cfapi.PlaceholderInfo{Size: 99, ModTime: fi.ModTime()}, fi) {
		t.Error("size change not detected")
	}
	if serverChanged(cfapi.PlaceholderInfo{Size: fi.Size(), ModTime: fi.ModTime()}, fi) {
		t.Error("identical file flagged as changed")
	}
	if !serverChanged(cfapi.PlaceholderInfo{Size: fi.Size(), ModTime: fi.ModTime().Add(time.Minute)}, fi) {
		t.Error("newer mtime not detected")
	}
}

func TestRemoteChangedPrefersETag(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	rec := newRecorder()
	w := bareWatcher(dir, rec.ops())
	rec.baselines["f.txt"] = "etag-1"
	// Same size/mtime (the heuristic would say unchanged) but a new ETag → changed.
	r := cfapi.PlaceholderInfo{Size: fi.Size(), ModTime: fi.ModTime(), ETag: "etag-2"}
	if !w.remoteChanged(r, fi, "f.txt") {
		t.Error("ETag change missed (same-size edit would be left stale)")
	}
	r.ETag = "etag-1"
	if w.remoteChanged(r, fi, "f.txt") {
		t.Error("matching ETag flagged as changed")
	}
}

func TestSuppress(t *testing.T) {
	w := bareWatcher(`C:\root`, Ops{})
	w.suppressDelete(`C:\root\Sub`)
	if !w.isSuppressed(`C:\root\sub`) {
		t.Error("case-insensitive match failed")
	}
	if !w.isSuppressed(`C:\root\sub\child.txt`) {
		t.Error("subtree match failed")
	}
	if w.isSuppressed(`C:\root\sub2`) {
		t.Error("sibling prefix wrongly suppressed")
	}
}

// --- FILE_NOTIFY_INFORMATION parsing ---------------------------------------

// notifyBuf encodes FILE_NOTIFY_INFORMATION records like ReadDirectoryChanges.
func notifyBuf(t *testing.T, recs []struct {
	action uint32
	name   string
}) []byte {
	t.Helper()
	var buf []byte
	for i, r := range recs {
		u := utf16.Encode([]rune(r.name))
		rec := make([]byte, 12+len(u)*2)
		if pad := len(rec) % 4; pad != 0 {
			rec = append(rec, make([]byte, 4-pad)...)
		}
		next := uint32(0)
		if i < len(recs)-1 {
			next = uint32(len(rec))
		}
		binary.LittleEndian.PutUint32(rec[0:], next)
		binary.LittleEndian.PutUint32(rec[4:], r.action)
		binary.LittleEndian.PutUint32(rec[8:], uint32(len(u)*2))
		for j, c := range u {
			binary.LittleEndian.PutUint16(rec[12+j*2:], c)
		}
		buf = append(buf, rec...)
	}
	return buf
}

func TestParseDispatch(t *testing.T) {
	rec := newRecorder()
	w := bareWatcher(`C:\root`, rec.ops())
	defer w.cancel()

	w.parse(notifyBuf(t, []struct {
		action uint32
		name   string
	}{
		{fileActionAdded, "new.txt"},
		{fileActionModified, `sub\edit.txt`},
		{fileActionRemoved, "gone.txt"},
		{fileActionAdded, "skip.tmp"}, // must be filtered out
	}))

	w.mu.Lock()
	_, up1 := w.upload[`C:\root\new.txt`]
	_, up2 := w.upload[`C:\root\sub\edit.txt`]
	_, del := w.delete[`C:\root\gone.txt`]
	_, skipped := w.upload[`C:\root\skip.tmp`]
	for _, tm := range w.upload {
		tm.Stop()
	}
	for _, tm := range w.delete {
		tm.Stop()
	}
	w.mu.Unlock()

	if !up1 || !up2 {
		t.Error("ADDED/MODIFIED did not schedule uploads")
	}
	if !del {
		t.Error("REMOVED did not schedule a delete")
	}
	if skipped {
		t.Error("temp file (.tmp) was not filtered")
	}
}

func TestParseRenamePair(t *testing.T) {
	rec := newRecorder()
	w := bareWatcher(`C:\root`, rec.ops())
	defer w.cancel()

	w.parse(notifyBuf(t, []struct {
		action uint32
		name   string
	}{
		{fileActionRenamedOld, "old.txt"},
		{fileActionRenamedNew, "new.txt"},
	}))

	select {
	case mv := <-rec.moved:
		if mv != [2]string{"old.txt", "new.txt"} {
			t.Errorf("move = %v", mv)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("rename pair did not dispatch a Move")
	}
}

func TestDeleteCancelledWhenPathReturns(t *testing.T) {
	rec := newRecorder()
	w := bareWatcher(`C:\root`, rec.ops())
	defer w.cancel()

	// REMOVED then ADDED for the same path (atomic save / rename shuffle): the
	// pending server delete must be cancelled.
	w.parse(notifyBuf(t, []struct {
		action uint32
		name   string
	}{{fileActionRemoved, "doc.txt"}}))
	w.parse(notifyBuf(t, []struct {
		action uint32
		name   string
	}{{fileActionAdded, "doc.txt"}}))

	w.mu.Lock()
	_, stillPending := w.delete[`C:\root\doc.txt`]
	for _, tm := range w.upload {
		tm.Stop()
	}
	w.mu.Unlock()
	if stillPending {
		t.Error("delete not cancelled by the path reappearing")
	}
}

// Windows reports a cross-directory move as REMOVED + ADDED. When the ADDED
// item is a placeholder whose identity names the REMOVED path, it IS that
// file: MOVE it on the server; never DELETE the source or upload the copy.
func TestParseTreatsCrossDirectoryMoveAsRename(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	newp := filepath.Join(root, "sub", "doc.txt")
	if err := os.WriteFile(newp, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f.setIdentity(newp, "doc.txt") // still names its OLD server path
	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.parse(notifyBuf(t, []struct {
		action uint32
		name   string
	}{
		{fileActionRemoved, "doc.txt"},
		{fileActionAdded, `sub\doc.txt`},
	}))

	select {
	case mv := <-rec.moved:
		if mv != [2]string{"doc.txt", "sub/doc.txt"} {
			t.Errorf("move = %v", mv)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("REMOVED+ADDED with a matching identity did not dispatch a MOVE")
	}
	w.mu.Lock()
	_, del := w.delete[filepath.Join(root, "doc.txt")]
	_, up := w.upload[newp]
	for _, tm := range w.upload {
		tm.Stop()
	}
	w.mu.Unlock()
	if del {
		t.Error("server delete still scheduled for the move's source")
	}
	if up {
		t.Error("upload scheduled for the moved placeholder")
	}
}

// A brand-new plain file that merely shares a name with a REMOVED one is not
// a move: the delete stands and the new file uploads.
func TestParseUnrelatedAddedKeepsDelete(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	newp := filepath.Join(root, "sub", "doc.txt")
	if err := os.WriteFile(newp, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markPlain(newp)
	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.parse(notifyBuf(t, []struct {
		action uint32
		name   string
	}{
		{fileActionRemoved, "doc.txt"},
		{fileActionAdded, `sub\doc.txt`},
	}))

	w.mu.Lock()
	_, del := w.delete[filepath.Join(root, "doc.txt")]
	_, up := w.upload[newp]
	for _, tm := range w.upload {
		tm.Stop()
	}
	for _, tm := range w.delete {
		tm.Stop()
	}
	w.mu.Unlock()
	if !del || !up {
		t.Errorf("plain file: delete scheduled=%v upload scheduled=%v, want both true", del, up)
	}
}

// A folder the user created locally is plain (no identity), but the server
// folders they moved into it are placeholders whose identities name the old
// location — moving that folder must be a MOVE, not a DELETE of everything
// inside it.
func TestParseTreatsPlainDirectoryMoveByChildren(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	newDir := filepath.Join(root, "archive", "New folder")
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(newDir, "2026.04")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	f.markPlain(newDir)                        // never converted
	f.setIdentity(child, "New folder/2026.04") // a server folder moved in earlier
	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.parse(notifyBuf(t, []struct {
		action uint32
		name   string
	}{
		{fileActionRemoved, "New folder"},
		{fileActionAdded, `archive\New folder`},
	}))

	select {
	case mv := <-rec.moved:
		if mv != [2]string{"New folder", "archive/New folder"} {
			t.Errorf("move = %v", mv)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("plain directory move with placeholder children did not dispatch a MOVE")
	}
	w.mu.Lock()
	_, del := w.delete[filepath.Join(root, "New folder")]
	for _, tm := range w.upload {
		tm.Stop()
	}
	w.mu.Unlock()
	if del {
		t.Error("server delete still scheduled for the moved folder")
	}
}

// The same move, one level deeper: the user's plain folder holds another
// plain folder ("New folder\Sub") and the server folders were dragged into
// THAT. Only the nested placeholder proves the move, so the probe has to
// look past the immediate children — a one-level probe read the whole
// subtree as a deletion and removed it from the server.
func TestParseTreatsNestedPlainDirectoryMoveByChildren(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	newDir := filepath.Join(root, "archive", "New folder")
	nested := filepath.Join(newDir, "Sub")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(nested, "2026.04")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	f.markPlain(newDir)                            // the user's own folder
	f.markPlain(nested)                            // …and the one they made inside it
	f.setIdentity(child, "New folder/Sub/2026.04") // a server folder moved in earlier
	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.parse(notifyBuf(t, []struct {
		action uint32
		name   string
	}{
		{fileActionRemoved, "New folder"},
		{fileActionAdded, `archive\New folder`},
	}))

	select {
	case mv := <-rec.moved:
		if mv != [2]string{"New folder", "archive/New folder"} {
			t.Errorf("move = %v", mv)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("nested placeholder did not prove the plain directory's move")
	}
	w.mu.Lock()
	_, del := w.delete[filepath.Join(root, "New folder")]
	for _, tm := range w.upload {
		tm.Stop()
	}
	w.mu.Unlock()
	if del {
		t.Error("server delete still scheduled for the moved folder")
	}
}

// The probe is bounded: it must not walk a huge tree on the event-pump
// goroutine. A placeholder buried deeper than the depth cap is not proof the
// probe is allowed to go looking for.
func TestPlainDirectoryProbeIsDepthBounded(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	newDir := filepath.Join(root, "archive", "New folder")
	deep := filepath.Join(newDir, "a", "b", "c", "d")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{newDir, filepath.Join(newDir, "a"), filepath.Join(newDir, "a", "b"),
		filepath.Join(newDir, "a", "b", "c")} {
		f.markPlain(p)
	}
	f.setIdentity(deep, "New folder/a/b/c/d")
	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	if old, ok := w.moveSourceFromChildren(newDir, []string{filepath.Join(root, "New folder")}); ok {
		t.Errorf("probe matched %q past the %d-level cap", old, moveProbeDepth)
	}
}

// --- reconcile (down-sync) ---------------------------------------------------

func TestReconcileCreatesAdditions(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "keep.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{
		ph("keep.txt", false, "e-keep", "f-keep"),
		ph("new.txt", false, "e-new", "f-new"),
		ph("sub", true, "e-sub", ""),
	}
	rec.baselines["keep.txt"] = "e-keep" // in sync, must not be touched
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	if _, err := os.Stat(filepath.Join(root, "new.txt")); err != nil {
		t.Error("server addition new.txt not created locally")
	}
	if fi, err := os.Stat(filepath.Join(root, "sub")); err != nil || !fi.IsDir() {
		t.Error("server addition sub/ not created locally")
	}
	if rec.baselines["new.txt"] != "e-new" {
		t.Errorf("baseline for new.txt = %q, want e-new", rec.baselines["new.txt"])
	}
	if rec.fileids["new.txt"] != "f-new" {
		t.Errorf("fileid for new.txt = %q, want f-new", rec.fileids["new.txt"])
	}
}

func TestReconcilePropagatesServerDelete(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	gone := filepath.Join(root, "gone.txt")
	dirty := filepath.Join(root, "dirty.txt")
	for _, p := range []string{gone, dirty} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.markDirty(dirty) // pending local upload — must survive

	rec := newRecorder()
	rec.listing[""] = nil // server says: nothing here
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Error("in-sync placeholder not removed on server delete")
	}
	if _, err := os.Stat(dirty); err != nil {
		t.Error("pending-upload file was wrongly deleted")
	}
}

func TestReconcileListErrorIsNotEmpty(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	keep := filepath.Join(root, "keep.txt")
	if err := os.WriteFile(keep, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := newRecorder()
	rec.listErr = os.ErrDeadlineExceeded // any listing failure
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	if _, err := os.Stat(keep); err != nil {
		t.Fatal("listing failure treated as empty directory — local file deleted")
	}
}

func TestReconcileDetectsServerRename(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	old := filepath.Join(root, "old.txt")
	if err := os.WriteFile(old, []byte("hydrated"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := newRecorder()
	rec.fileids["old.txt"] = "fid-1" // recorded when old.txt was created
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("renamed.txt", false, "e2", "fid-1")}
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	if _, err := os.Stat(filepath.Join(root, "renamed.txt")); err != nil {
		t.Fatal("renamed placeholder missing — rename fell back to delete+create")
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("old path still present after server rename")
	}
	f.mu.Lock()
	repointed := len(f.repointed) == 1
	f.mu.Unlock()
	if !repointed {
		t.Error("placeholder identity not repointed to the new remote path")
	}
	if rec.baselines["renamed.txt"] != "e2" {
		t.Error("baseline not recorded under the new path")
	}
	if _, ok := rec.fileids["old.txt"]; ok {
		t.Error("stale fileid for the old path not dropped")
	}
	if len(rec.deletes) != 0 {
		t.Error("server rename caused a spurious delete")
	}
}

func TestReconcileRefreshesChangedFile(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	doc := filepath.Join(root, "doc.txt")
	if err := os.WriteFile(doc, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := newRecorder()
	rec.baselines["doc.txt"] = "etag-v1"
	r := ph("doc.txt", false, "etag-v2", "fid") // server has a new version
	fi, _ := os.Stat(doc)
	r.Size, r.ModTime = fi.Size(), fi.ModTime() // same size/mtime: only the ETag differs
	rec.listing[""] = []cfapi.PlaceholderInfo{r}
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	f.mu.Lock()
	refreshed := len(f.refreshed) == 1
	f.mu.Unlock()
	if !refreshed {
		t.Fatal("same-size server edit not refreshed (stale local copy)")
	}
	if rec.baselines["doc.txt"] != "etag-v2" {
		t.Error("baseline not advanced to the refreshed version")
	}
}

func TestReconcileETagSubtreeSkip(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "a.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("sub", true, "etag-sub", "")}
	rec.listing["sub"] = []cfapi.PlaceholderInfo{ph("a.txt", false, "e-a", "f-a")}
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile() // pass 1: lists root + sub, records sub's collection ETag
	w.Reconcile() // pass 2: sub's ETag unchanged → its subtree must be skipped

	rec.mu.Lock()
	rootCalls, subCalls := rec.listCalls[""], rec.listCalls["sub"]
	rec.mu.Unlock()
	if rootCalls != 2 {
		t.Errorf("root listed %d times, want 2 (always re-listed)", rootCalls)
	}
	if subCalls != 1 {
		t.Errorf("sub listed %d times, want 1 (ETag subtree skip)", subCalls)
	}
}

// --- live watcher over a real directory --------------------------------------

func TestWatcherEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("debounce timing test")
	}
	f := installFakeCf(t)
	root := t.TempDir()
	rec := newRecorder()
	ops := rec.ops()
	ops.List = nil // no reconcile loop in this test
	w, err := New(context.Background(), root, "", 0, ops)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// Create: a brand-new (dirty) file must be uploaded and marked in-sync.
	p := filepath.Join(root, "a.txt")
	f.markDirty(p)
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-rec.uploaded:
		if got != "a.txt" {
			t.Fatalf("uploaded %q, want a.txt", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("create was not uploaded")
	}

	// Rename: must MOVE on the server, not delete+upload.
	if err := os.Rename(p, filepath.Join(root, "b.txt")); err != nil {
		t.Fatal(err)
	}
	select {
	case mv := <-rec.moved:
		if mv != [2]string{"a.txt", "b.txt"} {
			t.Fatalf("move = %v", mv)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("rename was not moved")
	}

	// Delete: must propagate to the server after the debounce.
	if err := os.Remove(filepath.Join(root, "b.txt")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-rec.deleted:
		if got != "b.txt" {
			t.Fatalf("deleted %q, want b.txt", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("delete was not propagated")
	}
}

// --- #580 aftermath: plain files healed by reconcile -------------------------

// healSetup builds a root with one local file the fake reports as PLAIN (no
// cloud state — the #580 flattening aftermath) and a server listing for it.
func healSetup(t *testing.T, etag, baseline string, matchMeta bool) (*fakeCf, *recorder, *Watcher, string) {
	t.Helper()
	f := installFakeCf(t)
	root := t.TempDir()
	doc := filepath.Join(root, "doc.txt")
	if err := os.WriteFile(doc, []byte("same bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markPlain(doc)

	rec := newRecorder()
	r := ph("doc.txt", false, etag, "fid-1")
	if matchMeta {
		fi, _ := os.Stat(doc)
		r.Size, r.ModTime = fi.Size(), fi.ModTime()
	} else {
		r.Size = 999 // server version demonstrably differs
	}
	rec.listing[""] = []cfapi.PlaceholderInfo{r}
	if baseline != "" {
		rec.baselines["doc.txt"] = baseline
	}
	w := bareWatcher(root, rec.ops())
	return f, rec, w, doc
}

// A flattened file whose size+mtime match the server listing, with the server
// unchanged since our last sync (baseline == listing ETag), is converted back
// to an in-sync placeholder — Explorer's perpetual "sync pending" arrows on it
// disappear without any transfer.
func TestReconcileHealsFlattenedFile(t *testing.T) {
	f, rec, w, doc := healSetup(t, "etag-v1", "etag-v1", true)
	defer w.cancel()

	w.Reconcile()

	f.mu.Lock()
	marked := append([]string(nil), f.marked...)
	f.mu.Unlock()
	if len(marked) != 1 || marked[0] != doc {
		t.Fatalf("flattened file not healed (marked=%v)", marked)
	}
	if id, _ := f.identityOf(doc); id != "doc.txt" {
		t.Errorf("healed with identity %q, want doc.txt", id)
	}
	if rec.baselines["doc.txt"] != "etag-v1" {
		t.Errorf("baseline = %q, want etag-v1", rec.baselines["doc.txt"])
	}
	if rec.fileids["doc.txt"] != "fid-1" {
		t.Errorf("fileid = %q, want fid-1", rec.fileids["doc.txt"])
	}
	if _, err := os.Stat(doc); err != nil {
		t.Error("file vanished during heal")
	}
	f.mu.Lock()
	notified := append([]string(nil), f.notified...)
	f.mu.Unlock()
	if len(notified) != 1 || notified[0] != doc {
		t.Errorf("shell not notified of the healed item (notified=%v) — Explorer keeps the stale glyph until a manual refresh", notified)
	}
}

// A plain file that does NOT match the server metadata is someone's pending
// work (a local edit the watcher never saw, or a newer server version) — it
// must not be stamped in-sync, uploaded, or deleted by the heal.
func TestReconcileLeavesMismatchedPlainFileAlone(t *testing.T) {
	f, rec, w, doc := healSetup(t, "etag-v2", "etag-v1", false)
	defer w.cancel()

	w.Reconcile()

	f.mu.Lock()
	marked := len(f.marked)
	f.mu.Unlock()
	if marked != 0 {
		t.Fatal("mismatched plain file was wrongly stamped in-sync")
	}
	rec.mu.Lock()
	ups, dels := len(rec.uploads), len(rec.deletes)
	rec.mu.Unlock()
	if ups != 0 || dels != 0 {
		t.Fatalf("heal performed server ops (uploads=%d deletes=%d)", ups, dels)
	}
	if _, err := os.Stat(doc); err != nil {
		t.Error("plain file deleted")
	}
}

// Size and mtime matching is not enough when the recorded baseline says the
// server has moved on since we last synced this file: the identical-looking
// metadata could be a coincidence, so the heal must decline.
func TestReconcileHealRespectsStaleBaseline(t *testing.T) {
	f, _, w, _ := healSetup(t, "etag-v2", "etag-v1", true)
	defer w.cancel()

	w.Reconcile()

	f.mu.Lock()
	marked := len(f.marked)
	f.mu.Unlock()
	if marked != 0 {
		t.Fatal("plain file stamped in-sync despite a stale baseline")
	}
}

// The ETag subtree skip must not starve the heal: with nothing changed
// server-side every subdirectory is skipped before its contents are listed, so
// a #580-flattened file deep in the tree would never meet healPlainFile. The
// FIRST pass after the watcher starts therefore ignores the skip; later passes
// use it as usual.
func TestReconcileFirstPassHealsInsideUnchangedSubtree(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	doc := filepath.Join(root, "sub", "doc.txt")
	if err := os.WriteFile(doc, []byte("same bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markPlain(doc)

	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("sub", true, "e-sub", "")}
	r := ph("doc.txt", false, "e-doc", "fid-1")
	r.Identity = []byte("sub/doc.txt")
	fi, _ := os.Stat(doc)
	r.Size, r.ModTime = fi.Size(), fi.ModTime()
	rec.listing["sub"] = []cfapi.PlaceholderInfo{r}
	// The subtree looks unchanged: sub's recorded collection ETag matches the
	// listing, and the file's baseline matches its ETag.
	rec.baselines["sub"] = "e-sub"
	rec.baselines["sub/doc.txt"] = "e-doc"
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	f.mu.Lock()
	marked := append([]string(nil), f.marked...)
	f.mu.Unlock()
	if len(marked) != 1 || marked[0] != doc {
		t.Fatalf("flattened file inside an unchanged subtree not healed on the first pass (marked=%v)", marked)
	}

	// A later pass goes back to skipping the unchanged subtree.
	rec.mu.Lock()
	before := rec.listCalls["sub"]
	rec.mu.Unlock()
	w.Reconcile()
	rec.mu.Lock()
	after := rec.listCalls["sub"]
	rec.mu.Unlock()
	if after != before {
		t.Errorf("second pass re-listed the unchanged subtree (%d -> %d) — ETag skip lost", before, after)
	}
}

// A change event on a file that needs no upload may still be a PIN change:
// Explorer's "Free up space" sets FILE_ATTRIBUTE_UNPINNED and waits for the
// provider to dehydrate. The watcher must offer such files to the settle path
// instead of ignoring them — otherwise the request hangs forever as
// sync-pending arrows (VM, 2026-08-19: "i just told it to free up space...
// says sync pending, where nimbo isnt syncing anything").
func TestChangeEventSettlesPendingUnpin(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	doc := filepath.Join(root, "doc.txt")
	if err := os.WriteFile(doc, []byte("uploaded already"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Clean (no upload needed) — the branch that used to just return.
	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleChange(doc)

	f.mu.Lock()
	settled := append([]string(nil), f.settleChecked...)
	f.mu.Unlock()
	if len(settled) != 1 || settled[0] != doc {
		t.Fatalf("clean file's change event not offered to the settle path (checked=%v)", settled)
	}
	rec.mu.Lock()
	ups := len(rec.uploads)
	rec.mu.Unlock()
	if ups != 0 {
		t.Fatalf("clean file wrongly uploaded (%d uploads)", ups)
	}
}

// --- pin hydration (issue #7) -----------------------------------------------

// Explorer's "Always keep on this device" (and Nimbo's own menu entry) only
// sets an attribute; the attribute-change event must make US download.
func TestPinnedChangeEventHydrates(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	p := filepath.Join(root, "map.tif")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f.markPinned(p)
	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleChange(p) // what the debounced ATTRIBUTES event runs

	// The download is queued for a worker, so it lands just after the call.
	deadline := time.Now().Add(3 * time.Second)
	for len(f.hydratedPaths()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := f.hydratedPaths(); len(got) != 1 || !strings.EqualFold(got[0], p) {
		t.Errorf("hydrated = %v, want [%s]", got, p)
	}
}

// Pins applied while Nimbo wasn't running are met by the reconcile sweep. The
// hook must be asynchronous: Reconcile holds reconMu for the whole pass, and
// a slow (or stuck) download must not stall every other reconcile consumer.
func TestReconcileHydratesPinnedFiles(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	p := filepath.Join(root, "x.txt")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f.markPinned(p)
	f.mu.Lock()
	f.hydrateGate = make(chan struct{})
	f.mu.Unlock()
	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("x.txt", false, "e1", "fid1")}
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	done := make(chan struct{})
	go func() {
		w.Reconcile()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Reconcile did not return while hydration was gated — the hook must not run synchronously")
	}
	if n := f.hydrateCompleted(); n != 0 {
		t.Fatalf("%d download(s) finished before the gate was opened — the hook ran synchronously", n)
	}

	f.mu.Lock()
	close(f.hydrateGate)
	f.mu.Unlock()

	deadline := time.Now().Add(3 * time.Second)
	for len(f.hydratedPaths()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := f.hydratedPaths(); len(got) != 1 {
		t.Errorf("hydrated = %v, want [%s]", got, p)
	}
}

// Hydration is bounded (hydrateWorkers at a time) and a path already in
// flight is not queued twice.
func TestHydratePinnedBoundedAndDeduped(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	var paths []string
	for _, n := range []string{"a", "b", "c"} {
		p := filepath.Join(root, n+".bin")
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		f.markPinned(p)
		paths = append(paths, p)
	}
	f.mu.Lock()
	f.hydrateGate = make(chan struct{})
	f.mu.Unlock()
	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	for _, p := range paths {
		w.requestHydration(p)
	}
	w.requestHydration(paths[0]) // duplicate
	time.Sleep(300 * time.Millisecond)
	if n := len(f.hydratedPaths()); n != hydrateWorkers {
		t.Errorf("in flight = %d, want %d", n, hydrateWorkers)
	}
	f.mu.Lock()
	close(f.hydrateGate)
	f.mu.Unlock()
	deadline := time.Now().Add(3 * time.Second)
	for len(f.hydratedPaths()) < 3 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := len(f.hydratedPaths()); n != 3 {
		t.Errorf("hydrated %d files, want 3 (no duplicates, none lost)", n)
	}
	// The fake is a no-op for a path it's already seen (delete then skip on
	// re-check), so without the dedup this would still show 3 hydrated — the
	// call count is what actually proves the duplicate never reached it.
	if n := f.hydrateCallCount(paths[0]); n != 1 {
		t.Errorf("%s: seam called %d times, want 1 (duplicate call not deduped)", paths[0], n)
	}
}

// The watcher owns the hydration work, not the callers: requests wait in a
// pending list drained by hydrateWorkers goroutines, rather than one parked
// goroutine per pinned file — a recursively pinned 50k-file tree used to park
// 50k goroutines on a 2-slot semaphore, 100-200 MB of stacks for work that
// runs two at a time. Nothing may be DROPPED to achieve that, though: the
// bounded channel that replaced those goroutines dropped every request past
// 258 on the assumption that a later reconcile pass meets a still-pinned file
// again. It does not — a pin is a local attribute change, so the directory's
// collection ETag never moves and the ETag subtree skip returns before the
// file is walked again. Measured: 268 pinned files hydrated 258, then 0 on
// every later pass, and "Always keep on this device" sat on "sync pending"
// until the next app restart.
func TestHydrationQueueNeverDropsARequest(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	const total = 300 // comfortably past the old hydrateWorkers+256 bound
	paths := make([]string, 0, total)
	for i := 0; i < total; i++ {
		p := filepath.Join(root, "f"+strconv.Itoa(i)+".bin")
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		f.markPinned(p)
		paths = append(paths, p)
	}
	f.mu.Lock()
	f.hydrateGate = make(chan struct{})
	f.mu.Unlock()
	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	for _, p := range paths {
		w.requestHydration(p) // must never block, however many are waiting
	}
	// Both workers are stuck in the gated download; the rest wait their turn.
	deadline := time.Now().Add(3 * time.Second)
	for len(f.hydratedPaths()) < hydrateWorkers && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := len(f.hydratedPaths()); n != hydrateWorkers {
		t.Fatalf("in flight = %d, want %d", n, hydrateWorkers)
	}

	f.mu.Lock()
	close(f.hydrateGate)
	f.mu.Unlock()
	deadline = time.Now().Add(20 * time.Second)
	for len(f.hydratedPaths()) < total && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := len(f.hydratedPaths()); n != total {
		t.Errorf("hydrated %d of %d requests — the surplus was dropped and nothing ever comes back for it", n, total)
	}
	if peak := f.hydratePeakInFlight(); peak > hydrateWorkers {
		t.Errorf("peak downloads in flight = %d, want at most %d", peak, hydrateWorkers)
	}
	for _, p := range paths {
		if n := f.hydrateCallCount(p); n != 1 {
			t.Errorf("%s: seam called %d times, want exactly 1", p, n)
		}
	}
	w.mu.Lock()
	left, pending := len(w.hydrating), len(w.hydratePending)
	w.mu.Unlock()
	if left != 0 || pending != 0 {
		t.Errorf("after draining: %d marked in flight, %d still pending; want 0 and 0", left, pending)
	}
}

// The provider's own hydrate callback reports every download it serves, so
// the pin path must not report a second one for the same bytes.
func TestHydrationDoesNotDoubleReportADownload(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	p := filepath.Join(root, "keep.bin")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f.markPinned(p)
	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.hydratePinned(p)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, r := range rec.reports {
		if r.kind == "download" && r.err == nil {
			t.Errorf("hydration reported a successful download (%v) — the provider's hydrate callback already does", r)
		}
	}
}

// A pinned file whose server copy also changed must be refreshed to the
// CURRENT content before it's downloaded: refreshing dehydrates the
// placeholder (the whole point is to drop stale local bytes so the next open
// re-fetches), so hydrating first would fetch content about to be discarded
// — or, if the refresh then failed, be this pass's only visible outcome.
func TestReconcileRefreshesBeforeHydratingPinned(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	p := filepath.Join(root, "x.txt")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f.markPinned(p)
	f.markDehydrated(p)
	rec := newRecorder()
	rec.baselines["x.txt"] = "e1"
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("x.txt", false, "e2", "fid1")} // server moved on: e1 -> e2
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	deadline := time.Now().Add(3 * time.Second)
	for f.eventIndex("hydrate:"+p) < 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	refreshAt, hydrateAt := f.eventIndex("refresh:"+p), f.eventIndex("hydrate:"+p)
	if refreshAt < 0 {
		t.Fatal("server-changed pinned file was never refreshed")
	}
	if hydrateAt < 0 {
		t.Fatal("pinned file was never hydrated")
	}
	if refreshAt >= hydrateAt {
		t.Errorf("refresh at index %d, hydrate at index %d — hydrated before (or racing) the refresh", refreshAt, hydrateAt)
	}
}

// A pinned file whose download keeps failing must back off instead of being
// retried — and re-reported — on every call.
func TestHydrateFailureBacksOff(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	p := filepath.Join(root, "bad.bin")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f.markHydrateErr(p, errors.New("boom"))
	origBase := retryBase
	retryBase = 200 * time.Millisecond
	t.Cleanup(func() { retryBase = origBase })
	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	countErrReports := func() int {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		n := 0
		for _, r := range rec.reports {
			if r.kind == "download" && r.err != nil {
				n++
			}
		}
		return n
	}

	w.hydratePinned(p)
	w.hydratePinned(p) // immediate retry — the backoff window hasn't elapsed

	if n := f.hydrateCallCount(p); n != 1 {
		t.Fatalf("seam called %d times, want 1 (second call not backed off)", n)
	}
	if n := countErrReports(); n != 1 {
		t.Fatalf("%d error reports, want 1", n)
	}

	time.Sleep(300 * time.Millisecond) // past the 200ms backoff
	w.hydratePinned(p)

	if n := f.hydrateCallCount(p); n != 2 {
		t.Fatalf("seam called %d times, want 2 (backoff never expired)", n)
	}
	if n := countErrReports(); n != 1 {
		t.Fatalf("%d error reports after the second failure, want 1 (later failures are log-only)", n)
	}
}

// Plain directories inside a mount (flatten victims) that are EMPTY are a
// permanent dead-end for every other heal: reconcile treats an empty dir as
// lazy-unpopulated and never lists it, so it never earns the etag baseline
// healPlainDirs demands — and Explorer reads a non-placeholder dir as
// never-synced, projecting pending arrows onto every ancestor (VM: 17 such
// dirs under Disk Info1\Smart kept the folder on arrows after everything else
// healed). Reconcile holds the server listing — proof enough: an in-both
// plain EMPTY dir is replaced with a real lazy placeholder…
func TestReconcileReplacesEmptyPlainDir(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Smart"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.markPlain(filepath.Join(root, "Smart"))
	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("Smart", true, "e-smart", "")}
	rec.listing["Smart"] = []cfapi.PlaceholderInfo{ph("data.csv", false, "e-csv", "f-csv")}
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	got := f.createdNames()
	if len(got) < 1 || got[0] != "Smart" {
		t.Fatalf("empty plain dir not recreated as a placeholder (created=%v)", got)
	}
	if fi, err := os.Stat(filepath.Join(root, "Smart")); err != nil || !fi.IsDir() {
		t.Fatalf("Smart missing after replacement: %v", err)
	}
	// The replacement must not stop at a LAZY placeholder — a not-in-sync dir
	// draws the same pending arrows the heal exists to remove. Reconcile is
	// online: populate the fresh dir from the server listing and mark it
	// in-sync, settled end to end.
	foundChild := false
	for _, n := range got {
		if n == "data.csv" {
			foundChild = true
		}
	}
	if !foundChild {
		t.Errorf("replacement did not populate the fresh dir from the listing (created=%v)", got)
	}
	f.mu.Lock()
	marked := append([]string(nil), f.marked...)
	f.mu.Unlock()
	dirMarked := false
	for _, m := range marked {
		if m == filepath.Join(root, "Smart") {
			dirMarked = true
		}
	}
	if !dirMarked {
		t.Errorf("replaced dir not marked in-sync (marked=%v) — it would wear lazy arrows until first opened", marked)
	}
	rec.mu.Lock()
	dels := len(rec.deletes)
	rec.mu.Unlock()
	if dels != 0 {
		t.Fatalf("replacement leaked a server delete (%d) — the local remove must be suppressed", dels)
	}
}

// …and an in-both plain POPULATED dir is converted in place (the adopt-proven
// conversion), keeping its contents untouched.
func TestReconcileConvertsPopulatedPlainDir(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	sub := filepath.Join(root, "Keep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "data.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markPlain(sub)
	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("Keep", true, "e-keep", "")}
	rec.listing["Keep"] = []cfapi.PlaceholderInfo{ph("data.txt", false, "e-d", "fd")}
	rec.baselines["Keep/data.txt"] = "e-d"
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	f.mu.Lock()
	marked := append([]string(nil), f.marked...)
	f.mu.Unlock()
	found := false
	for _, m := range marked {
		if m == sub {
			found = true
		}
	}
	if !found {
		t.Fatalf("populated plain dir not converted in place (marked=%v)", marked)
	}
	if b, err := os.ReadFile(filepath.Join(sub, "data.txt")); err != nil || string(b) != "x" {
		t.Fatalf("contents disturbed: %v", err)
	}
}

// --- issue #1: failed uploads must retry, invisibly-stuck files must rescue ---

// A transiently-failed upload must be retried automatically. Before this, a
// failed upload was reported once and the file ignored forever ("As if that
// file specifically is now ignored. Previous failure and no retry mechanism?"
// — GitHub issue #1).
func TestUploadFailureRetriedWithBackoff(t *testing.T) {
	ob, om := retryBase, retryMax
	retryBase, retryMax = 20*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { retryBase, retryMax = ob, om })

	f := installFakeCf(t)
	root := t.TempDir()
	doc := filepath.Join(root, "doc.txt")
	if err := os.WriteFile(doc, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDirty(doc)
	rec := newRecorder()
	rec.uploadFails = 2
	rec.uploadErr = errors.New("Put \"https://x\": http2: server sent GOAWAY")
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleChange(doc)

	select {
	case got := <-rec.uploaded:
		if got != "doc.txt" {
			t.Fatalf("uploaded %q, want doc.txt", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("failed upload never retried")
	}
	rec.mu.Lock()
	var failReports int
	for _, r := range rec.reports {
		if r.kind == "upload" && r.err != nil {
			failReports++
		}
	}
	rec.mu.Unlock()
	if failReports == 0 {
		t.Error("failures not reported to the activity feed")
	}
}

func TestRetryDelayGrowsAndCaps(t *testing.T) {
	if retryDelay(1) != retryBase {
		t.Errorf("first retry = %v, want %v", retryDelay(1), retryBase)
	}
	if retryDelay(2) != 2*retryBase {
		t.Errorf("second retry = %v, want %v", retryDelay(2), 2*retryBase)
	}
	if d := retryDelay(50); d != retryMax {
		t.Errorf("delay after many failures = %v, want capped at %v", d, retryMax)
	}
}

// Only one upload may run per path: change events landing while a (multi-hour)
// upload is in flight must not start a second concurrent upload of the same
// file — they re-arm one more pass after it finishes.
func TestInFlightUploadNotDuplicated(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	doc := filepath.Join(root, "big.bin")
	if err := os.WriteFile(doc, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDirty(doc)
	rec := newRecorder()
	gate := make(chan struct{})
	rec.uploadGate = gate
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	go w.handleChange(doc)
	// Wait for the first upload to be in flight (it blocks on the gate after
	// recording itself).
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec.mu.Lock()
		n := len(rec.uploads)
		rec.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first upload never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	w.handleChange(doc) // a change event during the upload
	rec.mu.Lock()
	n := len(rec.uploads)
	rec.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d concurrent uploads of the same path, want 1", n)
	}
	rec.mu.Lock()
	rec.uploadGate = nil
	rec.mu.Unlock()
	close(gate)
	<-rec.uploaded
	// The queued change re-arms one more upload after the first finishes.
	select {
	case <-rec.uploaded:
	case <-time.After(5 * time.Second):
		t.Fatal("change during in-flight upload was lost")
	}
}

// Reconcile must re-arm the upload of a dirty local-only file: a failed upload
// whose retry state died with the process (or whose change event was lost)
// would otherwise sit dirty forever.
func TestReconcileReschedulesDirtyLocalOnlyFile(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	doc := filepath.Join(root, "stuck.txt")
	if err := os.WriteFile(doc, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDirty(doc)
	rec := newRecorder()
	rec.listing[""] = nil // server doesn't have it
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	select {
	case got := <-rec.uploaded:
		if got != "stuck.txt" {
			t.Fatalf("uploaded %q, want stuck.txt", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dirty local-only file not rescued by reconcile")
	}
}

// The first reconcile pass after start must also rescue a dirty file that
// exists on BOTH sides (an edit whose upload failed before a restart).
func TestReconcileFirstPassRescuesDirtyInBothFile(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	doc := filepath.Join(root, "edited.txt")
	if err := os.WriteFile(doc, []byte("local edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDirty(doc)
	rec := newRecorder()
	r := ph("edited.txt", false, "e-1", "f-1")
	rec.listing[""] = []cfapi.PlaceholderInfo{r}
	rec.baselines["edited.txt"] = "e-1"
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	select {
	case got := <-rec.uploaded:
		if got != "edited.txt" {
			t.Fatalf("uploaded %q, want edited.txt", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dirty in-both file not rescued on the first pass")
	}
}

// A local sharing violation (the file is still being written — e.g. Explorer
// mid-copy of a 300GB file) is "try again soon", not a failure to report.
func TestSharingViolationRetriesQuietly(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	doc := filepath.Join(root, "copying.tif")
	if err := os.WriteFile(doc, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDirty(doc)
	rec := newRecorder()
	rec.uploadFails = 1
	rec.uploadErr = &os.PathError{Op: "open", Path: doc, Err: windows.ERROR_SHARING_VIOLATION}
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleChange(doc)

	rec.mu.Lock()
	var failReports int
	for _, r := range rec.reports {
		if r.err != nil {
			failReports++
		}
	}
	rec.mu.Unlock()
	if failReports != 0 {
		t.Error("busy-file error reported as a failure — should re-arm quietly like a lock hold")
	}
	w.mu.Lock()
	_, pending := w.upload[doc]
	w.mu.Unlock()
	if !pending {
		t.Error("busy file not rescheduled")
	}
}

// --- audit fixes: mid-upload edits, delete/rename resilience, rescue gaps ----

// An edit landing while that file's upload is in flight must NOT be stamped
// in-sync by the completing upload: the stamp would clear the dirty bit the
// edit set, the re-armed pass would see nothing to do, and the newest content
// would never upload (audit: confirmed data loss, destroyed on dehydrate).
func TestUploadDoesNotMarkInSyncOverMidUploadEdit(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	doc := filepath.Join(root, "doc.txt")
	if err := os.WriteFile(doc, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDirty(doc)
	rec := newRecorder()
	gate := make(chan struct{})
	rec.uploadGate = gate
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	done := make(chan struct{})
	go func() { w.handleChange(doc); close(done) }()
	// Wait until the upload is in flight, then edit the file under it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec.mu.Lock()
		n := len(rec.uploads)
		rec.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("upload never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // ensure a distinguishable mtime
	if err := os.WriteFile(doc, []byte("v2 - newer content"), 0o644); err != nil {
		t.Fatal(err)
	}
	close(gate)
	<-rec.uploaded
	<-done

	f.mu.Lock()
	marked := len(f.marked)
	f.mu.Unlock()
	if marked != 0 {
		t.Fatal("file edited during its upload was stamped in-sync — the edit is lost")
	}
	w.mu.Lock()
	_, rearmed := w.upload[doc]
	w.mu.Unlock()
	if !rearmed {
		t.Fatal("edited file not re-armed for another upload")
	}
}

// A transiently-failed server DELETE must retry — dropping it resurrects the
// deleted files at the next reconcile.
func TestDeleteRetriedOnTransientFailure(t *testing.T) {
	ob := retryBase
	retryBase = 20 * time.Millisecond
	t.Cleanup(func() { retryBase = ob })
	installFakeCf(t)
	root := t.TempDir()
	rec := newRecorder()
	rec.deleteFails = 1
	rec.deleteErr = errors.New("dial tcp: connection refused")
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleDelete(filepath.Join(root, "gone.txt"))

	select {
	case got := <-rec.deleted:
		if got != "gone.txt" {
			t.Fatalf("deleted %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("failed delete never retried")
	}
}

// A transient MOVE failure during a rename must retry the MOVE — the old
// fallback re-uploaded the file under its new name and left the old server
// copy behind (double storage, then a resurrect on reconcile).
func TestRenameMoveTransientFailureRetriesMove(t *testing.T) {
	ob := retryBase
	retryBase = 20 * time.Millisecond
	t.Cleanup(func() { retryBase = ob })
	installFakeCf(t)
	root := t.TempDir()
	newp := filepath.Join(root, "new.txt")
	if err := os.WriteFile(newp, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := newRecorder()
	rec.moveFails = 1
	rec.moveErr = errors.New("read tcp: connection reset by peer")
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleRename(filepath.Join(root, "old.txt"), newp)

	select {
	case mv := <-rec.moved:
		if mv != [2]string{"old.txt", "new.txt"} {
			t.Fatalf("move = %v", mv)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("transient MOVE failure never retried")
	}
	rec.mu.Lock()
	ups := len(rec.uploads)
	rec.mu.Unlock()
	if ups != 0 {
		t.Fatalf("transient MOVE failure fell back to a full upload (%d)", ups)
	}
}

// A MOVE that "fails" because the server already applied it (response lost)
// must be recognised via the destination, not degraded into an upload.
func TestRenameMoveLostResponseDetectedViaStat(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	newp := filepath.Join(root, "new.txt")
	if err := os.WriteFile(newp, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := newRecorder()
	rec.moveFails = 99
	rec.moveErr = errors.New("read tcp: connection reset by peer")
	rec.statExists = map[string]bool{"new.txt": true} // the server DID apply it
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleRename(filepath.Join(root, "old.txt"), newp)

	rec.mu.Lock()
	ups := len(rec.uploads)
	rec.mu.Unlock()
	if ups != 0 {
		t.Fatalf("lost-response rename degraded into an upload (%d)", ups)
	}
	// The placeholder identity must be repointed to the new name (success path).
	w.mu.Lock()
	pending := len(w.upload)
	w.mu.Unlock()
	if pending != 0 {
		t.Fatalf("lost-response rename left retries pending (%d)", pending)
	}
}

// A 404 on MOVE still means "never uploaded" and falls back to uploading.
func TestRenameMove404FallsBackToUpload(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	newp := filepath.Join(root, "new.txt")
	if err := os.WriteFile(newp, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDirty(newp)
	rec := newRecorder()
	rec.moveFails = 99
	rec.moveErr = errors.New(`MOVE "old.txt": server returned 404 Not Found: gone`)
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleRename(filepath.Join(root, "old.txt"), newp)

	select {
	case got := <-rec.uploaded:
		if got != "new.txt" {
			t.Fatalf("uploaded %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("404 MOVE did not fall back to upload")
	}
}

// An edit made just before the file was moved must survive the move. The
// repoint that follows a rename used to stamp MARK_IN_SYNC, which clears the
// pending-upload state — and handleRename has already cancelled the upload
// queued for the old path, so the edit became invisible to everything: no
// upload, and the next refresh would dehydrate the only copy of it away.
func TestRenameOfADirtyFileKeepsItsPendingUpload(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	newp := filepath.Join(root, "sub", "doc.txt")
	if err := os.WriteFile(newp, []byte("edited just before the move"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A locally edited file: the in-sync bit is clear AND it holds content the
	// server has not got. Both halves matter — the move clears the bit by
	// itself, so only the modified data tells a pending edit from a plain move.
	f.markDirty(newp)
	f.markModified(newp)
	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleRename(filepath.Join(root, "doc.txt"), newp)

	select {
	case mv := <-rec.moved:
		if mv != [2]string{"doc.txt", "sub/doc.txt"} {
			t.Errorf("move = %v", mv)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the rename never reached the server")
	}
	select {
	case got := <-rec.uploaded:
		if got != "sub/doc.txt" {
			t.Errorf("uploaded %q, want \"sub/doc.txt\"", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the edit made just before the move was never uploaded")
	}
	if !f.isDirty(newp) {
		t.Error("the repoint cleared the pending upload — the edit would be dehydrated away")
	}
	if got := f.keepStateRepoints(); len(got) != 1 || got[0] != newp {
		t.Errorf("keep-state repoints = %v, want [%s]", got, newp)
	}
}

// The Cloud Files filter clears a placeholder's in-sync bit on ANY rename or
// move, edit or no edit (measured live 2026-09-15, cfapi's
// TestRenameClearsInSyncButNotModifiedData: an online-only stub moved by
// another process went InSyncState 1 -> 0 with ModifiedDataSize still 0). So
// cfInspect reports NeedsUpload after EVERY move, and reading that as "there
// is an edit to send" sent every moved stub down the keep-state + upload
// branch: on the VM (2026-09-15) the upload of a just-moved online-only file
// found no baseline for its new path and parked the copy the MOVE had only
// just created as a "conflicted copy", leaving the local stub pointing at a
// server path that no longer existed. The predicate is unsynced local
// CONTENT, not the bit.
func TestMoveOfACleanStubIsMarkedInSyncNotUploaded(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	newp := filepath.Join(root, "sub", "a.bin")
	if err := os.WriteFile(newp, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDehydrated(newp) // online-only: there is no local data to send
	f.markNotInSync(newp)  // …and yet the move itself cleared its in-sync bit
	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleRename(filepath.Join(root, "a.bin"), newp)

	select {
	case mv := <-rec.moved:
		if mv != [2]string{"a.bin", "sub/a.bin"} {
			t.Errorf("move = %v", mv)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the move never reached the server")
	}
	select {
	case got := <-rec.uploaded:
		t.Fatalf("uploaded %q — a moved online-only stub has nothing to send, and the upload parks the server copy as a conflicted copy", got)
	case <-time.After(2 * time.Second):
	}
	if got := f.keepStateRepoints(); len(got) != 0 {
		t.Errorf("keep-state repoints = %v, want none — the stub must be stamped in-sync again", got)
	}
	if id, ok := f.identityOf(newp); !ok || id != "sub/a.bin" {
		t.Errorf("identity = %q, %v; want \"sub/a.bin\"", id, ok)
	}
	if f.isDirty(newp) {
		t.Error("the stub is still not in-sync — the repoint must restore the bit the move cleared")
	}
}

// A file that never was a placeholder (never uploaded, or flattened by a
// provider shutdown — Deck #580) carries content the server has not got by
// definition, so a move of it keeps its pending upload.
func TestMoveOfAPlainFileKeepsItsPendingUpload(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	newp := filepath.Join(root, "sub", "doc.txt")
	if err := os.WriteFile(newp, []byte("local bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markPlain(newp)
	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleRename(filepath.Join(root, "doc.txt"), newp)

	select {
	case <-rec.moved:
	case <-time.After(3 * time.Second):
		t.Fatal("the move never reached the server")
	}
	select {
	case got := <-rec.uploaded:
		if got != "sub/doc.txt" {
			t.Errorf("uploaded %q, want \"sub/doc.txt\"", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a plain file's content was never uploaded after the move")
	}
}

// A moved PLAIN directory — a folder the user created locally and then moved,
// which the pairing detector proves by the placeholders inside it — must never
// take the keep-state branch. It has no content of its own: the keep-state
// repoint fails on a non-placeholder (logged), and the upload it would
// schedule is a MKCOL of the folder the MOVE has only just created, which the
// server answers 405 and the activity feed shows as "folder created".
func TestMoveOfAPlainDirectoryOnlyRepointsIt(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	newDir := filepath.Join(root, "b")
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		t.Fatal(err)
	}
	f.markPlain(newDir) // never a placeholder: the user made this folder
	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleRename(filepath.Join(root, "a"), newDir)

	select {
	case mv := <-rec.moved:
		if mv != [2]string{"a", "b"} {
			t.Errorf("move = %v", mv)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the directory move never reached the server")
	}
	time.Sleep(2 * time.Second) // well past the upload debounce
	if got := f.keepStateRepoints(); len(got) != 0 {
		t.Errorf("keep-state repoints = %v, want none for a directory", got)
	}
	// The positive half: it IS repointed, with MARK_IN_SYNC. "Only repointed"
	// is two claims, and a fix that simply stopped touching the folder would
	// satisfy the negative ones while leaving every online-only child below it
	// fetching from a server path that no longer exists.
	if got := f.markInSyncRepoints(); len(got) != 1 || got[0] != newDir {
		t.Errorf("MARK_IN_SYNC repoints = %v, want exactly [%s]", got, newDir)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.mkdirs) != 0 {
		t.Errorf("MKCOLs = %v, want none — the MOVE already created the folder", rec.mkdirs)
	}
	if len(rec.uploads) != 0 {
		t.Errorf("uploads = %v, want none", rec.uploads)
	}
	for _, rp := range rec.reports {
		if rp.kind == "mkdir-remote" {
			t.Errorf("activity entry %v — a moved folder must not read as newly created", rp)
		}
	}
}

// A clean file is repointed exactly as before: MARK_IN_SYNC, no upload.
func TestRenameOfACleanFileStillMarksItInSync(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	newp := filepath.Join(root, "sub", "doc.txt")
	if err := os.WriteFile(newp, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleRename(filepath.Join(root, "doc.txt"), newp)

	select {
	case <-rec.moved:
	case <-time.After(3 * time.Second):
		t.Fatal("the rename never reached the server")
	}
	select {
	case got := <-rec.uploaded:
		t.Fatalf("uploaded %q — a clean file has nothing to send", got)
	case <-time.After(2 * time.Second):
	}
	if got := f.keepStateRepoints(); len(got) != 0 {
		t.Errorf("keep-state repoints = %v, want none for a clean file", got)
	}
	if id, ok := f.identityOf(newp); !ok || id != "sub/doc.txt" {
		t.Errorf("identity = %q, %v; want \"sub/doc.txt\"", id, ok)
	}
}

// After a directory rename/move, every placeholder beneath it must carry its
// NEW server path as identity, or an online-only child 404s on first open.
func TestDirectoryRenameRepointsDescendants(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	newDir := filepath.Join(root, "b")
	if err := os.MkdirAll(filepath.Join(newDir, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	// .htaccess exercises disguised-name escaping: its identity must be the
	// server's ESCAPED name, or hydration 404s exactly as the doc comment on
	// serverFor warns.
	for _, p := range []string{filepath.Join(newDir, "f1.txt"), filepath.Join(newDir, "inner", "f2.txt"), filepath.Join(newDir, "plain.txt"), filepath.Join(newDir, ".htaccess")} {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.markPlain(filepath.Join(newDir, "plain.txt")) // never uploaded: no identity to fix
	rec := newRecorder()
	w := bareWatcher(root, escapingOps(rec))
	defer w.cancel()

	w.handleRename(filepath.Join(root, "a"), newDir)

	select {
	case <-rec.moved:
	case <-time.After(3 * time.Second):
		t.Fatal("directory rename did not MOVE")
	}
	time.Sleep(200 * time.Millisecond) // the walk follows the MOVE
	f.mu.Lock()
	got := map[string]bool{}
	for _, p := range f.repointed {
		got[strings.ToLower(p)] = true
	}
	f.mu.Unlock()
	for _, want := range []string{newDir, filepath.Join(newDir, "f1.txt"), filepath.Join(newDir, "inner"), filepath.Join(newDir, "inner", "f2.txt"), filepath.Join(newDir, ".htaccess")} {
		if !got[strings.ToLower(want)] {
			t.Errorf("identity of %s not repointed", want)
		}
	}
	if got[strings.ToLower(filepath.Join(newDir, "plain.txt"))] {
		t.Error("plain (non-placeholder) file was repointed")
	}

	// The path-presence checks above prove the right set of descendants was
	// touched but not WHAT was written — a regression that stamped every
	// child with the directory's own identity, or dropped the nested path,
	// would stay green. Check the actual value written to each.
	wantIdentity := map[string]string{
		newDir:                                   "b",
		filepath.Join(newDir, "f1.txt"):          "b/f1.txt",
		filepath.Join(newDir, "inner"):           "b/inner",
		filepath.Join(newDir, "inner", "f2.txt"): "b/inner/f2.txt",
		filepath.Join(newDir, ".htaccess"):       "b/.htaccess.nimboesc",
	}
	for path, want := range wantIdentity {
		id, ok := f.identityOf(path)
		if !ok {
			t.Errorf("no identity recorded for %s", path)
			continue
		}
		if id != want {
			t.Errorf("identity of %s = %q, want %q", path, id, want)
		}
	}
	if _, ok := f.identityOf(filepath.Join(newDir, "plain.txt")); ok {
		t.Error("plain (non-placeholder) file has a recorded identity")
	}
}

// A file with unsynced local content inside a moved or renamed FOLDER must
// keep its pending upload, exactly as the moved file itself does. repointTree
// stamped MARK_IN_SYNC on every descendant, which clears the dirty bit: the
// edit then became invisible to the write-back gate (its old-path upload timer
// fires against a path that no longer exists, and reconcile's rescue sees an
// in-sync file), and the next refresh or "free up space" dehydrated its only
// copy. Before this branch a same-directory folder rename left descendants
// alone entirely, so this was a regression on an existing path.
func TestDirectoryMoveKeepsADirtyChildsPendingUpload(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	newDir := filepath.Join(root, "b")
	if err := os.MkdirAll(filepath.Join(newDir, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	clean := filepath.Join(newDir, "clean.txt")
	edited := filepath.Join(newDir, "inner", "edited.txt")
	for _, p := range []string{clean, edited} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.markDirty(edited)
	f.markModified(edited) // an edit still waiting to upload when the folder moved
	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleRename(filepath.Join(root, "a"), newDir)

	select {
	case <-rec.moved:
	case <-time.After(3 * time.Second):
		t.Fatal("the directory move never reached the server")
	}
	select {
	case got := <-rec.uploaded:
		if got != "b/inner/edited.txt" {
			t.Errorf("uploaded %q, want \"b/inner/edited.txt\"", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the edit inside the moved folder was never uploaded")
	}
	if !f.isDirty(edited) {
		t.Error("the descendant repoint cleared the pending upload — the edit would be dehydrated away")
	}
	if got := f.keepStateRepoints(); len(got) != 1 || !strings.EqualFold(got[0], edited) {
		t.Errorf("keep-state repoints = %v, want [%s]", got, edited)
	}
	if id, ok := f.identityOf(edited); !ok || id != "b/inner/edited.txt" {
		t.Errorf("identity of the edited child = %q, %v; want \"b/inner/edited.txt\"", id, ok)
	}
	// Its clean sibling is repointed the usual way.
	f.mu.Lock()
	var markedInSync bool
	for _, p := range f.repointed {
		if strings.EqualFold(p, clean) {
			markedInSync = true
		}
		if strings.EqualFold(p, edited) {
			t.Error("the edited child was stamped MARK_IN_SYNC as well")
		}
	}
	f.mu.Unlock()
	if !markedInSync {
		t.Error("the clean sibling was not repointed")
	}
}

// Deleting a file while its (multi-hour) upload is in flight must cancel the
// upload — otherwise the assembly recreates the file on the server after the
// DELETE, and reconcile resurrects it locally.
func TestDeleteDuringUploadCancelsIt(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	doc := filepath.Join(root, "big.bin")
	if err := os.WriteFile(doc, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDirty(doc)
	rec := newRecorder()
	gate := make(chan struct{})
	rec.uploadGate = gate
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	go w.handleChange(doc)
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec.mu.Lock()
		n := len(rec.uploads)
		rec.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("upload never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := os.Remove(doc); err != nil {
		t.Fatal(err)
	}
	w.handleDelete(doc)

	select {
	case got := <-rec.deleted:
		if got != "big.bin" {
			t.Fatalf("deleted %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("delete never reached the server")
	}
	rec.mu.Lock()
	cancelled := len(rec.cancelled)
	rec.mu.Unlock()
	if cancelled != 1 {
		t.Fatalf("in-flight upload not cancelled by the delete (cancelled=%d)", cancelled)
	}
}

// The dirty in-both rescue must run on EVERY reconcile pass, not just the
// first: a file whose change event was lost (buffer overflow) would otherwise
// stay stuck until the next app restart.
func TestReconcileRescuesDirtyInBothOnLaterPasses(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	doc := filepath.Join(root, "edited.txt")
	if err := os.WriteFile(doc, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := newRecorder()
	r := ph("edited.txt", false, "e-1", "f-1")
	rec.listing[""] = []cfapi.PlaceholderInfo{r}
	rec.baselines["edited.txt"] = "e-1"
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile() // pass 1: clean — nothing to do
	f.markDirty(doc)
	w.Reconcile() // pass 2: the file is now dirty with no live event

	select {
	case got := <-rec.uploaded:
		if got != "edited.txt" {
			t.Fatalf("uploaded %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dirty in-both file not rescued on a later pass")
	}
}

// Sync-excluded artifacts (the official client's journals, Desktop.ini) are
// plain LOCAL-ONLY files that Explorer renders as forever-pending inside a
// cloud root. Reconcile must offer them to the exclude path (convert +
// CF_PIN_STATE_EXCLUDED -> blank Status cell, VM-verified) instead of
// skipping them into permanent arrows.
func TestReconcileExcludesSkippedLocalFiles(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	ini := filepath.Join(root, "desktop.ini")
	if err := os.WriteFile(ini, []byte("[.ShellClassInfo]"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "real.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("real.txt", false, "e-r", "f-r")}
	rec.baselines["real.txt"] = "e-r"
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	f.mu.Lock()
	excluded := append([]string(nil), f.excluded...)
	f.mu.Unlock()
	if len(excluded) != 1 || excluded[0] != ini {
		t.Fatalf("skipName'd local file not offered to the exclude path (excluded=%v)", excluded)
	}
	if _, err := os.Stat(ini); err != nil {
		t.Fatal("desktop.ini deleted — exclusion must never remove the file")
	}
	rec.mu.Lock()
	ups := len(rec.uploads)
	rec.mu.Unlock()
	if ups != 0 {
		t.Fatalf("excluded file wrongly uploaded (%d)", ups)
	}
}

// --- cross-directory moves (issue #7) ---------------------------------------

// A move the filter reported must become a server MOVE, and the REMOVED event
// Windows raises for the move's source must NOT become a server DELETE.
func TestNotifyRenamedMovesInsteadOfDeleting(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	newp := filepath.Join(root, "sub", "doc.txt")
	if err := os.WriteFile(newp, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	// Windows' view of the move: REMOVED at the source …
	w.parse(notifyBuf(t, []struct {
		action uint32
		name   string
	}{{fileActionRemoved, "doc.txt"}}))
	// … and the filter's view: one rename-completion callback.
	w.NotifyRenamed(filepath.Join(root, "doc.txt"), newp)

	select {
	case mv := <-rec.moved:
		if mv != [2]string{"doc.txt", "sub/doc.txt"} {
			t.Errorf("move = %v", mv)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reported move did not dispatch a MOVE")
	}
	select {
	case d := <-rec.deleted:
		t.Fatalf("server DELETE of %q issued for a move", d)
	case <-time.After(2 * time.Second): // past deleteDebounce
	}
	w.mu.Lock()
	_, pending := w.delete[filepath.Join(root, "doc.txt")]
	w.mu.Unlock()
	if pending {
		t.Error("delete timer for the move's source still armed")
	}
}

// A REMOVED that arrives AFTER the callback (the debounce fires later) is
// still recognised as the move's source — and, having served that one
// REMOVED, the source key is consumed: it must not linger to also suppress
// an unrelated, later, genuine delete at the same (reused) path.
func TestRecentlyMovedBlocksLateDelete(t *testing.T) {
	root := t.TempDir()
	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()
	old := filepath.Join(root, "gone.txt")

	w.NotifyRenamed(old, filepath.Join(root, "sub", "gone.txt"))
	w.handleDelete(old) // what the debounce timer would run

	select {
	case d := <-rec.deleted:
		t.Fatalf("server DELETE of %q issued for a moved file", d)
	case <-time.After(500 * time.Millisecond):
	}
	if w.recentlyMoved(old) {
		t.Error("recentlyMoved key was not consumed by its own REMOVED")
	}
}

// The same rename reported twice — by the RENAMED pair and by the filter's
// callback — must produce exactly one MOVE.
func TestRenameReportedTwiceMovesOnce(t *testing.T) {
	root := t.TempDir()
	newp := filepath.Join(root, "new.txt")
	if err := os.WriteFile(newp, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.parse(notifyBuf(t, []struct {
		action uint32
		name   string
	}{
		{fileActionRenamedOld, "old.txt"},
		{fileActionRenamedNew, "new.txt"},
	}))
	w.NotifyRenamed(filepath.Join(root, "old.txt"), newp)

	time.Sleep(2 * time.Second)
	rec.mu.Lock()
	n := len(rec.moves)
	rec.mu.Unlock()
	if n != 1 {
		t.Errorf("MOVE count = %d, want 1", n)
	}
}

// ageMovedKeys simulates d elapsing for every key the watcher has recorded for
// a move, so a TTL can be tested without the test sleeping through it.
func ageMovedKeys(w *Watcher, d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for k, t := range w.moved {
		w.moved[k] = t.Add(-d)
	}
}

// A same-directory rename reported BOTH ways — the RENAMED pair first (it
// needs no identity probe, so it usually wins) and the filter's callback
// second — used to leave a source key behind for two minutes: the callback
// loses the claim, finds no delete timer to cancel (a same-directory rename
// has no REMOVED at all) and records the source anyway. A genuine delete of a
// NEW file created at that name within those two minutes was then swallowed,
// and the next reconcile pass pulled the file the user deleted back down.
func TestSameDirRenameReportedTwiceDoesNotSwallowALaterDelete(t *testing.T) {
	root := t.TempDir()
	newp := filepath.Join(root, "new.txt")
	if err := os.WriteFile(newp, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.parse(notifyBuf(t, []struct {
		action uint32
		name   string
	}{
		{fileActionRenamedOld, "old.txt"},
		{fileActionRenamedNew, "new.txt"},
	}))
	w.NotifyRenamed(filepath.Join(root, "old.txt"), newp) // the same rename, again
	select {
	case <-rec.moved:
	case <-time.After(3 * time.Second):
		t.Fatal("the rename never reached the server")
	}

	// Twenty seconds later the user saves a new file under the old name and
	// deletes it. The move's own REMOVED (if there were one) arrives within
	// milliseconds and its timer fires at 1.2 s, so a source key this old is
	// by construction a duplicate report's leftover.
	ageMovedKeys(w, 20*time.Second)
	w.parse(notifyBuf(t, []struct {
		action uint32
		name   string
	}{{fileActionRemoved, "old.txt"}}))
	select {
	case d := <-rec.deleted:
		if d != "old.txt" {
			t.Errorf("deleted %q, want \"old.txt\"", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a genuine delete was swallowed by the source key a duplicate rename report left behind")
	}
}

// The same leftover, by the other route: a cross-directory move arrives as
// REMOVED + ADDED in ONE batch, so the pairing detector claims it and cancels
// the delete timer itself; the filter's callback lands milliseconds later,
// loses the claim, finds nothing left to cancel, and records the source key.
func TestCrossDirMoveReportedTwiceDoesNotSwallowALaterDelete(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	newp := filepath.Join(root, "sub", "doc.txt")
	if err := os.WriteFile(newp, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.setIdentity(newp, "doc.txt") // the server still knows it at the old path
	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.parse(notifyBuf(t, []struct {
		action uint32
		name   string
	}{
		{fileActionRemoved, "doc.txt"},
		{fileActionAdded, `sub\doc.txt`},
	}))
	w.NotifyRenamed(filepath.Join(root, "doc.txt"), newp) // the callback, ms later
	select {
	case <-rec.moved:
	case <-time.After(3 * time.Second):
		t.Fatal("the move never reached the server")
	}

	ageMovedKeys(w, 20*time.Second)
	w.parse(notifyBuf(t, []struct {
		action uint32
		name   string
	}{{fileActionRemoved, "doc.txt"}}))
	select {
	case d := <-rec.deleted:
		if d != "doc.txt" {
			t.Errorf("deleted %q, want \"doc.txt\"", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a genuine delete was swallowed by the source key a duplicate move report left behind")
	}
}

// A moved source's suppression must expire with the one REMOVED it exists
// for — a NEW file later created at the vacated path and genuinely deleted
// must still reach the server (a lingering source key would let the server
// keep a file the user deleted).
func TestMovedSourceKeyIsConsumedByItsRemovedEvent(t *testing.T) {
	root := t.TempDir()
	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	old := filepath.Join(root, "old.txt")
	w.NotifyRenamed(old, filepath.Join(root, "sub", "old.txt"))

	// The move's own REMOVED: consumed by the source key, not a server delete.
	w.parse(notifyBuf(t, []struct {
		action uint32
		name   string
	}{{fileActionRemoved, "old.txt"}}))

	select {
	case d := <-rec.deleted:
		t.Fatalf("server DELETE of %q issued for the move's own REMOVED", d)
	case <-time.After(2 * time.Second): // past deleteDebounce
	}

	// A different file later created (and genuinely deleted) at the same
	// vacated path must not be swallowed by a lingering source key.
	w.parse(notifyBuf(t, []struct {
		action uint32
		name   string
	}{{fileActionRemoved, "old.txt"}}))

	select {
	case <-rec.deleted:
	case <-time.After(3 * time.Second):
		t.Fatal("genuine delete of a new file at the vacated path was suppressed")
	}
}

// If the move's REMOVED arrives before the filter's callback, cancelling the
// pending delete timer is enough — no source key should be recorded (there
// is no later REMOVED left to consume it).
func TestNotifyRenamedAfterRemovedCancelsWithoutLingering(t *testing.T) {
	root := t.TempDir()
	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	old := filepath.Join(root, "old.txt")
	w.parse(notifyBuf(t, []struct {
		action uint32
		name   string
	}{{fileActionRemoved, "old.txt"}}))

	w.NotifyRenamed(old, filepath.Join(root, "sub", "old.txt"))

	select {
	case d := <-rec.deleted:
		t.Fatalf("server DELETE of %q issued after its REMOVED was cancelled by the callback", d)
	case <-time.After(2 * time.Second): // past deleteDebounce
	}
	if w.recentlyMoved(old) {
		t.Error("a source key lingered though its REMOVED was already cancelled")
	}
}

// If the filter's callback arrives before the RENAMED pair for the very same
// rename, the pair's beginRename loses the claim (one MOVE only) and must
// forget the source key the callback recorded — proof that no REMOVED is
// coming for it.
func TestCallbackBeforeRenamePairForgetsSource(t *testing.T) {
	root := t.TempDir()
	newp := filepath.Join(root, "new.txt")
	if err := os.WriteFile(newp, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	old := filepath.Join(root, "old.txt")
	w.NotifyRenamed(old, newp)

	w.parse(notifyBuf(t, []struct {
		action uint32
		name   string
	}{
		{fileActionRenamedOld, "old.txt"},
		{fileActionRenamedNew, "new.txt"},
	}))

	time.Sleep(2 * time.Second)
	rec.mu.Lock()
	n := len(rec.moves)
	rec.mu.Unlock()
	if n != 1 {
		t.Errorf("MOVE count = %d, want 1", n)
	}
	if w.recentlyMoved(old) {
		t.Error("callback's source key lingered after the RENAMED pair proved no REMOVED is coming")
	}
}

// A move reported AGAIN inside the pair-claim window must still do its delete
// bookkeeping. The claim exists only so the SAME rename — reported both as a
// RENAMED pair and as the filter's callback, milliseconds apart — is pushed
// once; returning early on a repeat left the repeat's own REMOVED unclaimed,
// so a move, an undo and a redo inside the window DELETED the file on the
// server. That is the only copy when the destination is an online-only stub:
// its data lived at the source the DELETE just removed.
//
// The repeat itself is still deduplicated here — these are plain files, so the
// deferred placement check that fix wave 4 added to that path has no
// placeholder identity to judge the placement by and leaves them alone.
// TestRepeatedMoveThenModifiedEventDoesNotUploadTheStub is the placeholder
// version, where the third move IS pushed.
func TestRepeatedMoveInsideClaimWindowStillSuppressesItsDelete(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	x := filepath.Join(root, "doc.txt")
	y := filepath.Join(root, "sub", "doc.txt")
	if err := os.WriteFile(y, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.NotifyRenamed(x, y) // the move …
	w.handleDelete(x)     // … and its own REMOVED, consumed by the source key
	select {
	case <-rec.moved:
	case <-time.After(3 * time.Second):
		t.Fatal("the move never reached the server")
	}

	w.NotifyRenamed(y, x) // the user undoes it …
	w.handleDelete(y)
	select {
	case <-rec.moved:
	case <-time.After(3 * time.Second):
		t.Fatal("the undo never reached the server")
	}

	w.NotifyRenamed(x, y) // … and redoes it, inside the claim window
	w.handleDelete(x)     // the redo's REMOVED must not become a server DELETE

	select {
	case d := <-rec.deleted:
		t.Fatalf("server DELETE of %q issued for a move repeated inside the claim window", d)
	case <-time.After(500 * time.Millisecond):
	}
	rec.mu.Lock()
	moves, deletes := len(rec.moves), len(rec.deletes)
	rec.mu.Unlock()
	if deletes != 0 {
		t.Errorf("deletes = %d, want 0", deletes)
	}
	if moves != 2 {
		t.Errorf("moves = %d, want 2 (the move and its undo — the repeat is deduped)", moves)
	}
}

// When the repeat of a move finds the source's REMOVED already pending,
// cancelling that timer settles it — and any source key an earlier report of
// the same move recorded must be dropped with it. Left behind, it would
// swallow the next genuine delete at that path for movedSourceTTL: a new file
// created where the moved one used to be, then deleted, would stay on the
// server forever.
func TestRepeatedMoveCancellingItsRemovedForgetsTheSourceKey(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	x := filepath.Join(root, "doc.txt")
	y := filepath.Join(root, "sub", "doc.txt")
	if err := os.WriteFile(y, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.NotifyRenamed(x, y) // the callback first: it records a source key
	w.parse(notifyBuf(t, []struct {
		action uint32
		name   string
	}{{fileActionRemoved, "doc.txt"}})) // the move's REMOVED arms a delete timer
	w.NotifyRenamed(x, y) // the same move reported again: cancels that timer

	w.mu.Lock()
	_, lingering := w.moved[strings.ToLower(x)]
	w.mu.Unlock()
	if lingering {
		t.Error("source key lingered though the move's REMOVED was already cancelled")
	}

	// A different file created at the vacated path and genuinely deleted must
	// still reach the server.
	w.parse(notifyBuf(t, []struct {
		action uint32
		name   string
	}{{fileActionRemoved, "doc.txt"}}))
	select {
	case <-rec.deleted:
	case <-time.After(3 * time.Second):
		t.Fatal("a genuine delete at the vacated path was swallowed by a lingering source key")
	}
}

// A placeholder found where the server has nothing, whose identity names a
// DIFFERENT server path, was moved here while the watcher wasn't running:
// MOVE it on the server, keep it locally.
func TestReconcileMovesPlaceholderWithForeignIdentity(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(root, "b", "x.txt")
	if err := os.WriteFile(moved, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f.setIdentity(moved, "x.txt") // the server still has it at the root
	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("b", true, "etag-b", "")}
	rec.listing["b"] = nil
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	select {
	case mv := <-rec.moved:
		if mv != [2]string{"x.txt", "b/x.txt"} {
			t.Errorf("move = %v", mv)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("foreign identity did not become a MOVE")
	}
	if _, err := os.Stat(moved); err != nil {
		t.Error("moved placeholder was removed locally")
	}
}

// A file that is BOTH dirty and moved (edited, the upload failed, then moved
// while Nimbo was closed) must be MOVEd on the server, not uploaded at its new
// path: the rescue that uploads stuck-dirty files ran first, so the server kept
// its copy at the old path — the next pass of the old parent re-created a
// placeholder for it locally, and the user was left with the file twice.
func TestReconcileMovesADirtyPlaceholderInsteadOfDuplicatingIt(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(root, "b", "x.txt")
	if err := os.WriteFile(moved, []byte("edited before the move"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.setIdentity(moved, "x.txt") // the server still has it at the root
	f.markDirty(moved)
	f.markModified(moved) // …and it holds an edit that never uploaded
	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("b", true, "etag-b", "")}
	rec.listing["b"] = nil
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	select {
	case mv := <-rec.moved:
		if mv != [2]string{"x.txt", "b/x.txt"} {
			t.Errorf("move = %v", mv)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a dirty file with a foreign identity was not MOVEd — its server copy is orphaned at the old path")
	}
	select {
	case got := <-rec.uploaded:
		if got != "b/x.txt" {
			t.Errorf("uploaded %q, want \"b/x.txt\"", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the pending edit was never uploaded after the move")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.moves) != 1 {
		t.Errorf("moves = %d, want 1", len(rec.moves))
	}
	for _, u := range rec.uploads {
		if u == "x.txt" {
			t.Error("uploaded at the OLD server path as well — that is the duplicate")
		}
	}
}

// A reconcile pass overlapping a live move must leave that path to the move.
// Both the watcher's move detectors and reconcile's foreign-identity check
// look at the same evidence (an identity naming another server path), so a
// pass landing mid-move used to issue a SECOND MOVE for it: whichever lost
// the race 404s, and that surfaces as a bogus "missing on the server" error
// for an online-only file — or a redundant full re-upload for a hydrated one.
func TestReconcileLeavesAPathWithAMoveInFlightAlone(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(root, "b", "x.txt")
	if err := os.WriteFile(moved, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f.setIdentity(moved, "x.txt") // the server still has it at the root
	rec := newRecorder()
	rec.moveGate = make(chan struct{})
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("b", true, "etag-b", "")}
	rec.listing["b"] = nil
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	go w.handleRename(filepath.Join(root, "x.txt"), moved)
	deadline := time.Now().Add(3 * time.Second)
	for !w.moveInFlight(moved) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !w.moveInFlight(moved) {
		t.Fatal("the move never went in flight")
	}

	done := make(chan struct{})
	go func() {
		w.Reconcile()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Reconcile blocked behind the in-flight move — it issued a second MOVE for the same path")
	}

	close(rec.moveGate)
	select {
	case mv := <-rec.moved:
		if mv != [2]string{"x.txt", "b/x.txt"} {
			t.Errorf("move = %v", mv)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the move never completed")
	}
	time.Sleep(300 * time.Millisecond) // a second MOVE would land about now
	rec.mu.Lock()
	moves := len(rec.moves)
	var errs []reportRec
	for _, rp := range rec.reports {
		if rp.err != nil {
			errs = append(errs, rp)
		}
	}
	rec.mu.Unlock()
	if moves != 1 {
		t.Errorf("MOVE count = %d, want 1", moves)
	}
	if len(errs) != 0 {
		t.Errorf("error reports = %v, want none", errs)
	}
}

// inMove has to cover the whole retry CHAIN, not one request. A transient
// MOVE failure schedules a retry; if the mark were cleared in between, a
// reconcile pass landing in that gap reads exactly the same evidence (an
// identity naming another server path) and issues a SECOND MOVE for the same
// file. Whichever lands second 404s: a bogus "X is missing on the server" for
// an online-only file, or a redundant full re-upload for a hydrated one.
func TestReconcileInAMoveRetryGapIssuesNoSecondMove(t *testing.T) {
	ob := retryBase
	retryBase = 1500 * time.Millisecond // long enough to reconcile inside the gap
	t.Cleanup(func() { retryBase = ob })
	f := installFakeCf(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(root, "b", "x.txt")
	if err := os.WriteFile(moved, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f.setIdentity(moved, "x.txt") // the server still has it at the root
	rec := newRecorder()
	rec.moveFails = 1
	rec.moveErr = errors.New("read tcp: connection reset by peer")
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("b", true, "etag-b", "")}
	rec.listing["b"] = nil
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	go w.handleRename(filepath.Join(root, "x.txt"), moved)

	// Wait for the first attempt to fail — the retry is now pending.
	deadline := time.Now().Add(3 * time.Second)
	for {
		rec.mu.Lock()
		left := rec.moveFails
		rec.mu.Unlock()
		if left == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !w.moveInFlight(moved) {
		t.Fatal("the move stopped counting as in flight while its retry was still pending")
	}

	w.Reconcile() // lands in the retry gap

	select {
	case mv := <-rec.moved:
		if mv != [2]string{"x.txt", "b/x.txt"} {
			t.Errorf("move = %v", mv)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the retried MOVE never reached the server")
	}
	time.Sleep(500 * time.Millisecond) // a second MOVE would land about now
	rec.mu.Lock()
	moves := len(rec.moves)
	rec.mu.Unlock()
	if moves != 1 {
		t.Errorf("MOVE count = %d, want 1 (the retry only)", moves)
	}
	if w.moveInFlight(moved) {
		t.Error("the move mark was never released once the chain resolved")
	}
}

// When the MOVE 404s and the local copy is an online-only stub, there is
// nothing to upload: say so once instead of retrying a doomed upload forever
// (the issue #7 log: 37 attempts against a path the bug had deleted).
func TestMove404WithDehydratedStubDoesNotUpload(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	newp := filepath.Join(root, "stub.txt")
	if err := os.WriteFile(newp, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDehydrated(newp)
	rec := newRecorder()
	rec.moveFails = 99
	rec.moveErr = errors.New(`MOVE "old.txt": server returned 404 Not Found: gone`)
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleRename(filepath.Join(root, "old.txt"), newp)

	select {
	case got := <-rec.uploaded:
		t.Fatalf("uploaded %q — an online-only stub has no data to send", got)
	case <-time.After(2 * time.Second):
	}
	rec.mu.Lock()
	var reported int
	for _, r := range rec.reports {
		if r.kind == "move" && r.err != nil {
			reported++
		}
	}
	rec.mu.Unlock()
	if reported != 1 {
		t.Errorf("reported %d errors for the unrecoverable move, want exactly 1", reported)
	}
	// Nothing left to move and no local data: the stub's identity is repointed
	// to its own current path so the next pass reads it as an ordinary
	// server-side delete instead of rediscovering the same foreign identity.
	if id, ok := f.identityOf(newp); !ok || id != "stub.txt" {
		t.Errorf("identity = %q, %v; want repointed to \"stub.txt\"", id, ok)
	}
}

// reconcile found the placeholder itself (not via handleRename/NotifyRenamed),
// so it already knows nothing needs uploading (NeedsUpload == false) — that's
// WHY it read as a foreign-identity move rather than a pending upload. A 404
// on the resulting MOVE must not be silently dropped just because the in-sync
// gate would otherwise no-op: the destination is forced through the
// Mkdir/Upload branch, saving the only local copy of the file.
func TestReconcileMove404UploadsHydratedCopy(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(root, "b", "x.txt")
	if err := os.WriteFile(moved, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f.setIdentity(moved, "x.txt") // the server thinks it's still at the root
	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("b", true, "etag-b", "")}
	rec.listing["b"] = nil
	rec.moveFails = 99
	rec.moveErr = errors.New(`MOVE "x.txt": server returned 404 Not Found: gone`)
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	select {
	case got := <-rec.uploaded:
		if got != "b/x.txt" {
			t.Errorf("uploaded %q, want \"b/x.txt\"", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("404 move of a hydrated copy never fell back to a forced upload")
	}
	if _, err := os.Stat(moved); err != nil {
		t.Error("the only local copy of the file was removed")
	}
	// The repoint at the end of a FORCED upload stamps the identity with the
	// server name the upload used — MarkInSync alone would not (it only sets
	// the bit on something that is already a placeholder), and the file would
	// be left in sync naming the dead source, so a later "free up space"
	// would make every open hydrate from a 404. It lands just AFTER the upload
	// the channel above announces, so wait for it instead of racing it (an
	// immediate read failed roughly one run in twenty).
	deadline := time.Now().Add(3 * time.Second)
	for {
		id, ok := f.identityOf(moved)
		if ok && id == "b/x.txt" {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("identity = %q, %v; want \"b/x.txt\"", id, ok)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	rec.mu.Lock()
	uploads := append([]string(nil), rec.uploads...)
	rec.mu.Unlock()
	if len(uploads) != 1 {
		t.Errorf("uploads = %v, want exactly one — the repoint must settle it, not re-arm it", uploads)
	}
}

// TestReconcileStubWithGoneSourceReportsOnce is the on-demand analogue of
// TestMove404WithDehydratedStubDoesNotUpload, driven through reconcile's
// foreign-identity path rather than handleRename, and carried across a
// SECOND pass to pin down the ruled end state: the unrecoverable move is
// reported exactly once (never re-reported every pass, the issue #7 "37
// attempts" symptom), and once its identity is repointed to its own current
// path, the next pass reads it as an ordinary server-side delete and removes
// the stub locally — there was never any data here to lose.
func TestReconcileStubWithGoneSourceReportsOnce(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(root, "b", "x.txt")
	if err := os.WriteFile(moved, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f.setIdentity(moved, "x.txt")
	f.markDehydrated(moved)
	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("b", true, "etag-b", "")}
	rec.listing["b"] = nil
	rec.moveFails = 99
	rec.moveErr = errors.New(`MOVE "x.txt": server returned 404 Not Found: gone`)
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	select {
	case got := <-rec.uploaded:
		t.Fatalf("uploaded %q — an online-only stub has no data to send", got)
	case <-time.After(2 * time.Second):
	}
	countReports := func() int {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		n := 0
		for _, r := range rec.reports {
			if r.kind == "move" && r.err != nil {
				n++
			}
		}
		return n
	}
	if n := countReports(); n != 1 {
		t.Fatalf("reported %d errors after the first pass, want exactly 1", n)
	}
	if id, ok := f.identityOf(moved); !ok || id != "b/x.txt" {
		t.Fatalf("identity = %q, %v; want repointed to \"b/x.txt\"", id, ok)
	}
	if _, err := os.Stat(moved); err != nil {
		t.Fatal("the stub was removed locally after only the first pass")
	}
	// rec.moves only ever records a MOVE that the server accepted, and this
	// one never does (it 404s every time) — so "no further MOVE attempt" is
	// checked via the fail-counter instead: it should have been decremented
	// exactly once so far (one attempt, on the first pass).
	rec.mu.Lock()
	remaining := rec.moveFails
	rec.mu.Unlock()
	if remaining != 98 {
		t.Fatalf("moveFails remaining = %d after the first pass, want 98 (exactly one MOVE attempt)", remaining)
	}

	// Second pass: the identity now matches where the file already sits
	// ("b/x.txt"), so the foreign-identity check no longer fires, no MOVE is
	// attempted again, and the file is picked up as a plain in-both-missing
	// item on the server side — the same "not a rename — propagate the
	// server-side delete locally" path any other server-deleted file takes.
	w.Reconcile()

	if n := countReports(); n != 1 {
		t.Errorf("reported %d errors after the second pass, want still exactly 1 (no re-report)", n)
	}
	rec.mu.Lock()
	remaining = rec.moveFails
	moves := len(rec.moves)
	rec.mu.Unlock()
	if remaining != 98 {
		t.Errorf("moveFails remaining = %d after the second pass, want still 98 (no further MOVE attempt)", remaining)
	}
	if moves != 0 {
		t.Errorf("moves = %d, want 0 (this MOVE never once succeeded)", moves)
	}
	if _, err := os.Stat(moved); !os.IsNotExist(err) {
		t.Error("the stub should be gone locally after the second pass — the ruled end state, since it held no data anyway")
	}
}

// moveServer runs inside a reconcile pass, which holds reconMu for its
// duration — every down-sync trigger arriving meanwhile is dropped by
// Reconcile's TryLock. A forced upload can take hours on a big file (and
// there can be one per hydrated member of a vanished subtree), so it must
// run off the reconcile pass entirely, on the debounce timer's own goroutine
// like every other upload in this file — not synchronously inside moveServer.
func TestForcedUploadDoesNotBlockReconcile(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(root, "b", "x.txt")
	if err := os.WriteFile(moved, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f.setIdentity(moved, "x.txt") // the server thinks it's still at the root
	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("b", true, "etag-b", "")}
	rec.listing["b"] = nil
	rec.moveFails = 99
	rec.moveErr = errors.New(`MOVE "x.txt": server returned 404 Not Found: gone`)
	gate := make(chan struct{})
	rec.uploadGate = gate
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	start := time.Now()
	w.Reconcile() // the gate is still closed throughout this call
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Reconcile took %v with the upload gate closed — a forced upload blocked the reconcile pass", elapsed)
	}

	close(gate)
	select {
	case got := <-rec.uploaded:
		if got != "b/x.txt" {
			t.Errorf("uploaded %q, want \"b/x.txt\"", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("forced upload never ran once the gate opened")
	}
}

// --- fix wave 3: a MODIFIED event on a file the filter merely un-synced -----
//
// A cross-directory move delivers FILE_ACTION_MODIFIED for the DESTINATION as
// well as REMOVED/ADDED (measured live 2026-09-15 alongside the in-sync bit it
// clears), and parse dispatches MODIFIED straight to the upload debounce
// without the move pairing ADDED gets. So 800ms after every move, the
// write-back path is asked to judge a file whose in-sync bit is clear and
// whose content the server already has. Judging it by that bit uploaded it —
// on the VM, against a destination the MOVE had only just created, which
// parked the just-moved server copy as a conflicted copy and left the file at
// BOTH paths.

// modifiedEvent drives one FILE_ACTION_MODIFIED record through parse, exactly
// as the event pump does, and waits out the upload debounce.
func modifiedEvent(t *testing.T, w *Watcher, rel string) {
	t.Helper()
	w.parse(notifyBuf(t, []struct {
		action uint32
		name   string
	}{
		{fileActionModified, rel},
	}))
	time.Sleep(uploadDebounce + 400*time.Millisecond)
}

func TestModifiedEventOnAnUnmodifiedMovedFileRestoresInSync(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	p := filepath.Join(root, "a.bin")
	if err := os.WriteFile(p, []byte("server bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markNotInSync(p) // the move cleared the bit; the content is untouched

	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	modifiedEvent(t, w, "a.bin")

	rec.mu.Lock()
	uploads := append([]string(nil), rec.uploads...)
	rec.mu.Unlock()
	if len(uploads) != 0 {
		t.Errorf("uploaded %v — the file holds nothing the server has not got", uploads)
	}
	if got := f.inSyncedPaths(); len(got) != 1 || got[0] != p {
		t.Errorf("in-sync restored for %v, want exactly [%s] — otherwise the arrows never clear", got, p)
	}
}

func TestModifiedEventOnAnEditedFileStillUploads(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	p := filepath.Join(root, "a.bin")
	if err := os.WriteFile(p, []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDirty(p)            // in-sync cleared AND unsynced local content behind it
	f.setIdentity(p, "a.bin") // and it IS where the server has it

	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	modifiedEvent(t, w, "a.bin")

	select {
	case up := <-rec.uploaded:
		if up != "a.bin" {
			t.Errorf("uploaded %q, want a.bin", up)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a real local edit was never uploaded")
	}
	if got := f.inSyncedPaths(); len(got) != 0 {
		t.Errorf("in-sync restored for %v — that bit IS the pending upload", got)
	}
}

func TestModifiedEventOnAPlainFileStillUploads(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	p := filepath.Join(root, "a.bin")
	if err := os.WriteFile(p, []byte("brand new"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markPlain(p) // never uploaded: no placeholder at all

	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	modifiedEvent(t, w, "a.bin")

	select {
	case up := <-rec.uploaded:
		if up != "a.bin" {
			t.Errorf("uploaded %q, want a.bin", up)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a brand-new local file was never uploaded")
	}
}

// The belt-and-braces half: an ONLINE-ONLY stub has no local content at all,
// so whatever the dirty predicate says about it, the write-back path must not
// send it. Uploading one means hydrating it first just to push the same bytes
// back — the FETCH_DATA + GET + PUT the VM's verbose log caught.
func TestModifiedEventOnAnOnlineOnlyStubNeverUploads(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	p := filepath.Join(root, "a.bin")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDehydrated(p)
	// markDirty, not markNotInSync: the gate under test is the ONLINE half of
	// (!ch.NeedsUpload || online), so the file has to read as needing an
	// upload or the first half carries the test and the second could be
	// deleted unnoticed.
	f.markDirty(p)

	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	modifiedEvent(t, w, "a.bin")

	rec.mu.Lock()
	uploads := append([]string(nil), rec.uploads...)
	rec.mu.Unlock()
	if len(uploads) != 0 {
		t.Errorf("uploaded %v — an online-only stub holds no local data to send", uploads)
	}
}

// T2b from the VM retest, in unit form: three moves of the same stub inside
// the 15s claim window (the third deduped by the pair claim, so no
// finishRename runs for it), then the FILE_ACTION_MODIFIED the last move's
// destination gets.
//
// Fix wave 3 stopped that MODIFIED uploading the stub to a path the MOVE had
// only just created (which parked the just-moved server copy as a conflicted
// copy and left the file at BOTH src and dst). The retest then caught what it
// did not cover: the third move was dropped for good. The file sat at src on
// this PC and at dst everywhere else, indefinitely — reconcile never looks,
// because the deduped move changed nothing on the server and the ETag subtree
// skip hides a local-only divergence until the next full sweep. So the
// deduplicated report now gets a deferred placement check, and it is that
// check which pushes the third MOVE.
func TestRepeatedMoveThenModifiedEventDoesNotUploadTheStub(t *testing.T) {
	f := installFakeCf(t)
	shortenRenameVerify(t)
	root := t.TempDir()
	for _, d := range []string{"src", "dst"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	x := filepath.Join(root, "src", "a.bin")
	y := filepath.Join(root, "dst", "a.bin")
	if err := os.WriteFile(x, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDehydrated(x)
	f.setIdentity(x, "dst/a.bin") // the server still has it where it came from

	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.NotifyRenamed(y, x) // move 1: dst -> src
	w.handleDelete(y)
	select {
	case <-rec.moved:
	case <-time.After(3 * time.Second):
		t.Fatal("the first move never reached the server")
	}
	waitForMoveSettled(t, w, x) // the repoint lands before the mark clears
	movePlaceholder(t, f, x, y)
	w.NotifyRenamed(x, y) // move 2: back again
	w.handleDelete(x)
	select {
	case <-rec.moved:
	case <-time.After(3 * time.Second):
		t.Fatal("the second move never reached the server")
	}
	waitForMoveSettled(t, w, y)
	movePlaceholder(t, f, y, x)
	w.NotifyRenamed(y, x) // move 3: deduped by the pair claim — no finishRename
	w.handleDelete(y)
	select {
	case <-rec.moved:
	case <-time.After(3 * time.Second):
		t.Fatal("the deduplicated third move never reached the server: the file is at src here and at dst everywhere else")
	}
	// The repoint that follows that MOVE is a MARK_IN_SYNC: mark the file
	// not-in-sync before it lands and the heal below has nothing to restore.
	waitForMoveSettled(t, w, x)

	// …and the FILE_ACTION_MODIFIED the destination gets for the same rename.
	f.markNotInSync(x)
	modifiedEvent(t, w, `src\a.bin`)

	rec.mu.Lock()
	uploads := append([]string(nil), rec.uploads...)
	moves := append([][2]string(nil), rec.moves...)
	deletes := len(rec.deletes)
	rec.mu.Unlock()
	if len(uploads) != 0 {
		t.Errorf("uploaded %v — the stub was PUT to a path the MOVE already owns", uploads)
	}
	if len(moves) != 3 {
		t.Fatalf("moves = %v, want 3 (the third pushed by the deferred placement check)", moves)
	}
	if moves[2] != [2]string{"dst/a.bin", "src/a.bin"} {
		t.Errorf("third move = %v, want [dst/a.bin src/a.bin]", moves[2])
	}
	if deletes != 0 {
		t.Errorf("deletes = %d, want 0", deletes)
	}
	// Nothing else is going to touch this item, so the heal is the only thing
	// that clears its arrows.
	if got := f.inSyncedPaths(); len(got) != 1 || got[0] != x {
		t.Errorf("in-sync restored for %v, want exactly [%s]", got, x)
	}
}

// --- fix wave 3: reconcile's in-both branch --------------------------------

// reconcileHealOps wires a single in-both file with a known server ETag.
func reconcileHealFixture(t *testing.T, serverETag, baseline string) (*fakeCf, *recorder, *Watcher, string) {
	t.Helper()
	f := installFakeCf(t)
	root := t.TempDir()
	p := filepath.Join(root, "a.bin")
	if err := os.WriteFile(p, []byte("server bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.setIdentity(p, "a.bin")
	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{
		{Name: "a.bin", Size: 12, ModTime: time.Now(), Identity: []byte("a.bin"), ETag: serverETag},
	}
	rec.baselines["a.bin"] = baseline
	w := bareWatcher(root, rec.ops())
	return f, rec, w, p
}

func TestReconcileRestoresInSyncOnAnUnmodifiedMovedFile(t *testing.T) {
	f, rec, w, p := reconcileHealFixture(t, "etag-1", "etag-1")
	defer w.cancel()
	f.markNotInSync(p) // a move cleared the bit; the server has not moved on

	w.Reconcile()
	time.Sleep(uploadDebounce + 400*time.Millisecond) // let any rescue fire

	rec.mu.Lock()
	uploads := append([]string(nil), rec.uploads...)
	rec.mu.Unlock()
	if len(uploads) != 0 {
		t.Errorf("uploaded %v — the ETag says the server already has these bytes", uploads)
	}
	if got := f.inSyncedPaths(); len(got) != 1 || got[0] != p {
		t.Errorf("in-sync restored for %v, want exactly [%s] — no event is ever coming for this file", got, p)
	}
}

func TestReconcileLeavesAChangedServerFileToTheRefresh(t *testing.T) {
	f, rec, w, p := reconcileHealFixture(t, "etag-2", "etag-1")
	defer w.cancel()
	f.markNotInSync(p) // not in sync, but the server HAS moved on

	w.Reconcile()
	time.Sleep(uploadDebounce + 400*time.Millisecond) // let any rescue fire

	rec.mu.Lock()
	uploads := append([]string(nil), rec.uploads...)
	rec.mu.Unlock()
	if len(uploads) != 0 {
		t.Errorf("uploaded %v — a clean local copy is never a push", uploads)
	}
	// The heal runs on the DATA question alone (placeholder, not in sync,
	// nothing local to lose) — the server's ETag has no say in it. Gating it on
	// ETag == baseline used to leave this file in the one state nothing can
	// act on: the heal declined it, and the refresh below is VERIFY_IN_SYNC,
	// which the driver refuses for a not-in-sync placeholder. Every pass
	// logged "skipped: edited locally just now" and changed nothing, forever.
	if got := f.inSyncedPaths(); len(got) != 1 || got[0] != p {
		t.Errorf("in-sync restored for %v, want exactly [%s]", got, p)
	}
	// With the bit back, the refresh that follows can actually run — and its
	// VERIFY_IN_SYNC still closes the edit-in-between race.
	if got := f.refreshedPaths(); len(got) != 1 || got[0] != p {
		t.Errorf("refreshed %v, want exactly [%s] — the server copy moved on and ours is clean", got, p)
	}
	if idx := f.eventIndex("refresh:" + p); idx == -1 {
		t.Error("the refresh branch was never reached")
	}
}

func TestReconcileStillRescuesAnEditedInBothFile(t *testing.T) {
	f, rec, w, p := reconcileHealFixture(t, "etag-1", "etag-1")
	defer w.cancel()
	f.markDirty(p) // a real edit whose upload never happened

	w.Reconcile()

	select {
	case up := <-rec.uploaded:
		if up != "a.bin" {
			t.Errorf("uploaded %q, want a.bin", up)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the dirty in-both rescue stopped rescuing real edits")
	}
	if got := f.inSyncedPaths(); len(got) != 0 {
		t.Errorf("in-sync restored for %v — that bit IS the pending upload", got)
	}
}

// A directory placeholder is deliberately NOT in sync — that is what makes the
// shell ask it to populate — and marking one in-sync leaves it enumerating
// EMPTY forever (cfapi.TestInSyncDirStillPopulates pins the driver behaviour).
// Folders get FILE_ACTION_MODIFIED constantly, including for every move made
// INTO them (the live trace shows MODIFIED for the destination folder next to
// MODIFIED for the file), so the in-sync heal meets directories all day and
// must leave every one of them alone.
func TestModifiedEventOnADirectoryNeverMarksItInSync(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	dir := filepath.Join(root, "sub")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f.markNotInSync(dir) // every lazy directory placeholder looks like this

	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	modifiedEvent(t, w, "sub")

	if got := f.inSyncedPaths(); len(got) != 0 {
		t.Errorf("in-sync restored for %v — a directory marked in-sync never populates again", got)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.mkdirs) != 0 {
		t.Errorf("MKCOLs = %v, want none — it is already a placeholder", rec.mkdirs)
	}
}

// --- fix wave 4: a deduplicated rename report verifies its placement -------

// movePlaceholder renames a placeholder on disk the way another process does
// it, and carries its cloud state across with it: the identity and the
// hydration state live in the placeholder's own metadata, not in a table keyed
// by path, so a test that renames a file without moving them leaves the fake
// answering for the OLD path — a stale identity the next reader believes.
func movePlaceholder(t *testing.T, f *fakeCf, from, to string) {
	t.Helper()
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.identities[strings.ToLower(from)]; ok {
		f.identities[strings.ToLower(to)] = id
		delete(f.identities, strings.ToLower(from))
	}
	if f.dehydrated[strings.ToLower(filepath.ToSlash(from))] {
		delete(f.dehydrated, strings.ToLower(filepath.ToSlash(from)))
		f.dehydrated[strings.ToLower(filepath.ToSlash(to))] = true
	}
}

// shortenRenameVerify makes the deferred placement check fire in milliseconds
// rather than the second it waits in production.
func shortenRenameVerify(t *testing.T) {
	t.Helper()
	old := renameVerifyDelay
	renameVerifyDelay = 50 * time.Millisecond
	t.Cleanup(func() { renameVerifyDelay = old })
}

// waitForMoveSettled blocks until no server MOVE onto path is running any
// more. finishRename repoints the placeholder BEFORE the in-flight mark is
// cleared, so this is also how a test waits for the repoint: the MOVE landing
// on rec.moved only says the request returned.
func waitForMoveSettled(t *testing.T, w *Watcher, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for w.moveInFlight(path) {
		if time.Now().After(deadline) {
			t.Fatal("the move never settled")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForMoveInFlight blocks until a server MOVE onto path is running.
func waitForMoveInFlight(t *testing.T, w *Watcher, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !w.moveInFlight(path) {
		if time.Now().After(deadline) {
			t.Fatal("the move never went in flight")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The ordinary case the pair claim exists for: ONE move, reported twice — the
// filter's rename-completion callback and the RENAMED pair, milliseconds
// apart. The loser must not push a second MOVE. Its deferred check waits for
// the winner's MOVE to finish and then finds the placeholder already pointing
// at where the server has it.
func TestDuplicateReportOfOneMovePushesOneMove(t *testing.T) {
	f := installFakeCf(t)
	shortenRenameVerify(t)
	root := t.TempDir()
	newp := filepath.Join(root, "new.txt")
	if err := os.WriteFile(newp, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.setIdentity(newp, "old.txt") // the server still knows it at the old path

	rec := newRecorder()
	rec.moveGate = make(chan struct{}) // hold the winner's MOVE in flight
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	old := filepath.Join(root, "old.txt")
	w.NotifyRenamed(old, newp) // the callback claims the pair
	waitForMoveInFlight(t, w, newp)
	w.parse(notifyBuf(t, []struct {
		action uint32
		name   string
	}{
		{fileActionRenamedOld, "old.txt"},
		{fileActionRenamedNew, "new.txt"},
	})) // the same rename again: deduped -> a deferred placement check

	close(rec.moveGate)
	select {
	case mv := <-rec.moved:
		if mv != [2]string{"old.txt", "new.txt"} {
			t.Errorf("move = %v, want [old.txt new.txt]", mv)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the move never reached the server")
	}
	// A second MOVE would land a delay plus a poll tick after the first
	// cleared inMove — and one issued while the gate was still shut would have
	// been recorded the moment it opened.
	time.Sleep(2*renameVerifyDelay + renameVerifyPoll + 300*time.Millisecond)
	rec.mu.Lock()
	moves := append([][2]string(nil), rec.moves...)
	rec.mu.Unlock()
	if len(moves) != 1 {
		t.Errorf("moves = %v, want exactly one — the duplicate report pushed a MOVE of its own", moves)
	}
}

// An item that is gone by the time the check looks is nothing to judge:
// whatever happened to it next — deleted, moved on again — brings its own
// event, and a MOVE derived from an identity that no longer describes anything
// would be a guess.
func TestVerifyPlacementIgnoresAVanishedItem(t *testing.T) {
	f := installFakeCf(t)
	shortenRenameVerify(t)
	root := t.TempDir()
	newp := filepath.Join(root, "new.txt")
	if err := os.WriteFile(newp, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.setIdentity(newp, "old.txt") // misplaced: a check that found it WOULD move it

	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	old := filepath.Join(root, "old.txt")
	w.beginRename(old, newp)   // the first reporter claimed this pair …
	w.NotifyRenamed(old, newp) // … so this one is deduped -> a deferred check
	// …and the item is gone before that check gets to look at it.
	if err := os.Remove(newp); err != nil {
		t.Fatal(err)
	}

	time.Sleep(4*renameVerifyDelay + renameVerifyPoll + 300*time.Millisecond)
	rec.mu.Lock()
	moves := append([][2]string(nil), rec.moves...)
	var errs []reportRec
	for _, rp := range rec.reports {
		if rp.err != nil {
			errs = append(errs, rp)
		}
	}
	rec.mu.Unlock()
	if len(moves) != 0 {
		t.Errorf("moves = %v, want none for an item that is no longer there", moves)
	}
	if len(errs) != 0 {
		t.Errorf("error reports = %v, want none", errs)
	}
}

// Two deduplicated reports for one path — all three detectors describing the
// same move — must cost ONE check, not one each: two checks waking together
// read the same evidence and race each other's MOVE, and the loser 404s.
func TestVerifyPlacementIsDedupedPerPath(t *testing.T) {
	f := installFakeCf(t)
	shortenRenameVerify(t)
	root := t.TempDir()
	newp := filepath.Join(root, "new.txt")
	if err := os.WriteFile(newp, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.setIdentity(newp, "old.txt")

	rec := newRecorder()
	rec.moveGate = make(chan struct{}) // a second check's MOVE would queue here too
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	old := filepath.Join(root, "old.txt")
	w.beginRename(old, newp)   // the first reporter claimed the pair
	w.NotifyRenamed(old, newp) // deduped: check #1
	w.NotifyRenamed(old, newp) // deduped again, while #1 is still pending

	waitForMoveInFlight(t, w, newp)
	close(rec.moveGate)
	select {
	case mv := <-rec.moved:
		if mv != [2]string{"old.txt", "new.txt"} {
			t.Errorf("move = %v, want [old.txt new.txt]", mv)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the deduplicated report's placement check never pushed the move")
	}
	time.Sleep(2*renameVerifyDelay + renameVerifyPoll + 300*time.Millisecond)
	rec.mu.Lock()
	moves := append([][2]string(nil), rec.moves...)
	rec.mu.Unlock()
	if len(moves) != 1 {
		t.Errorf("moves = %v, want exactly one", moves)
	}
}

// While the claimant's MOVE is still running, the check must wait it out,
// however long that takes: mid-move the identity still names the OLD server
// path — exactly the evidence a live move reads — and a second MOVE for the
// same file makes one of the two 404. Once the move resolves, finishRename has
// already repointed the identity, so there is nothing left to do.
func TestVerifyPlacementWaitsForTheInFlightMove(t *testing.T) {
	f := installFakeCf(t)
	shortenRenameVerify(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	newp := filepath.Join(root, "b", "x.txt")
	if err := os.WriteFile(newp, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f.setIdentity(newp, "x.txt")

	rec := newRecorder()
	rec.moveGate = make(chan struct{})
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	old := filepath.Join(root, "x.txt")
	w.beginRename(old, newp)     // the first reporter claims it …
	go w.handleRename(old, newp) // … and its MOVE sits on the gate
	waitForMoveInFlight(t, w, newp)

	w.NotifyRenamed(old, newp) // the second report: deduped -> a deferred check
	time.Sleep(10*renameVerifyDelay + renameVerifyPoll)
	w.mu.Lock()
	pending := w.verifying[strings.ToLower(newp)]
	w.mu.Unlock()
	if !pending {
		t.Error("the placement check gave up while the claimant's MOVE was still in flight")
	}

	close(rec.moveGate)
	select {
	case <-rec.moved:
	case <-time.After(3 * time.Second):
		t.Fatal("the claimant's move never completed")
	}
	time.Sleep(2*renameVerifyDelay + renameVerifyPoll + 300*time.Millisecond)
	rec.mu.Lock()
	moves := append([][2]string(nil), rec.moves...)
	rec.mu.Unlock()
	if len(moves) != 1 {
		t.Errorf("moves = %v, want exactly one — finishRename had already repointed it", moves)
	}
}

// A detach parks a folder the account no longer has (Deck #557) and suppresses
// the paths it touches; that can land while a deferred placement check is
// still waiting, i.e. after its entry gates have already run. The check must
// look again before it pushes anything, or it MOVEs a file another part of the
// watcher is in the middle of taking out of the cloud folder.
func TestVerifyPlacementStopsIfTheItemIsSuppressedWhileItWaits(t *testing.T) {
	f := installFakeCf(t)
	// Deliberately NOT shortened: the full delay gives the suppression a wide,
	// deterministic window after the entry gates and before the identity read.
	root := t.TempDir()
	newp := filepath.Join(root, "new.txt")
	if err := os.WriteFile(newp, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.setIdentity(newp, "old.txt") // misplaced: without the re-check it WOULD be moved

	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	old := filepath.Join(root, "old.txt")
	w.beginRename(old, newp)   // the first reporter claimed the pair …
	w.NotifyRenamed(old, newp) // … so this one is deduped -> a check, now waiting

	time.Sleep(200 * time.Millisecond) // it is past its entry gates
	w.suppressDelete(newp)             // and now the path is OUR doing, not the user's

	time.Sleep(renameVerifyDelay + renameVerifyPoll + 500*time.Millisecond)
	rec.mu.Lock()
	moves := append([][2]string(nil), rec.moves...)
	rec.mu.Unlock()
	if len(moves) != 0 {
		t.Errorf("moves = %v, want none — the path was suppressed while the check waited", moves)
	}
}

// --- fix wave 5: the identity guard ----------------------------------------

// setRenameVerifyDelay overrides the deferred placement check's delay for one
// test (shortenRenameVerify is the usual, fast form; a few tests need it LONG
// so that what happens before the check fires can be asserted on its own).
func setRenameVerifyDelay(t *testing.T, d time.Duration) {
	t.Helper()
	old := renameVerifyDelay
	renameVerifyDelay = d
	t.Cleanup(func() { renameVerifyDelay = old })
}

// The route the ratified truncate-to-zero tie-break leaves open: a HYDRATED,
// empty placeholder whose in-sync bit a move cleared reads as holding unsynced
// local content, and its identity still names the path the server keeps it at.
// Uploading it here would PUT a second copy at the new name — and MarkInSync
// never rewrites an identity, so that copy would be in sync pointing at a
// server path it does not occupy: the file at both paths, permanently.
func TestModifiedEventOnAMovedHydratedEmptyFileIsNotUploaded(t *testing.T) {
	f := installFakeCf(t)
	// The guard now starts a placement check as it refuses the upload; this
	// test is about what handleChange itself does, so hold that check off
	// until well past the assertions.
	setRenameVerifyDelay(t, 10*time.Second)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "dst", "a.bin")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Hydrated (never markDehydrated), empty, bit cleared by the move, and the
	// tie-break's verdict on it: unsynced local content.
	f.markDirty(p)
	f.setIdentity(p, "src/a.bin") // the server still has it at src

	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	modifiedEvent(t, w, `dst\a.bin`)

	rec.mu.Lock()
	uploads := append([]string(nil), rec.uploads...)
	moves := append([][2]string(nil), rec.moves...)
	rec.mu.Unlock()
	if len(uploads) != 0 {
		t.Errorf("uploaded %v — a second copy at a path the server does not hold it at", uploads)
	}
	if len(moves) != 0 {
		t.Errorf("moves = %v — handleChange pushes nothing itself; the move path owns this file", moves)
	}
	if got := f.inSyncedPaths(); len(got) != 0 {
		t.Errorf("in-sync restored for %v — it is not where the server has it, so that would be a lie", got)
	}
}

// A forced upload is moveServer's 404 fallback: the source was never on the
// server, so uploading at the NEW name is the whole plan and the guard must
// not stand in its way.
func TestForcedUploadStillRunsPastAForeignIdentity(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	p := filepath.Join(root, "a.bin")
	if err := os.WriteFile(p, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDirty(p)
	f.setIdentity(p, "old/a.bin")

	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.forceChange(p) // what moveServer's 404 branch does

	select {
	case up := <-rec.uploaded:
		if up != "a.bin" {
			t.Errorf("uploaded %q, want a.bin", up)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the 404 fallback's forced upload was blocked by the identity guard")
	}
}

// T2b end to end with a HYDRATED, empty file — the exact population the
// ratified tie-break calls dirty. Two pushed moves (each repointed by
// finishRename, each followed by its redundant 0-byte PUT at the name the
// server now uses), then the deduped third move, whose MODIFIED event must
// push NOTHING: no finishRename has run, so the placeholder still names the
// old path. Wave 4's placement check then MOVEs it, and only the upload that
// follows THAT — at the new server path — is allowed.
func TestRepeatedMoveOfAHydratedEmptyFileUploadsOnlyAtTheNewName(t *testing.T) {
	f := installFakeCf(t)
	setRenameVerifyDelay(t, 3*time.Second) // fires well after the MODIFIED event
	root := t.TempDir()
	for _, d := range []string{"src", "dst"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	x := filepath.Join(root, "src", "a.bin")
	y := filepath.Join(root, "dst", "a.bin")
	if err := os.WriteFile(x, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDirty(x)                // the bit the move cleared + the tie-break's verdict
	f.setIdentity(x, "dst/a.bin") // the server still has it where it came from

	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.NotifyRenamed(y, x) // move 1: dst -> src
	w.handleDelete(y)
	select {
	case <-rec.moved:
	case <-time.After(3 * time.Second):
		t.Fatal("the first move never reached the server")
	}
	waitForMoveSettled(t, w, x)
	select { // finishRename kept its pending upload and sent it at the new name
	case up := <-rec.uploaded:
		if up != "src/a.bin" {
			t.Errorf("uploaded %q after move 1, want src/a.bin", up)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the kept upload never ran after move 1")
	}

	movePlaceholder(t, f, x, y)
	f.markDirty(y) // the filter clears the bit again; the tie-break agrees again
	w.NotifyRenamed(x, y)
	w.handleDelete(x)
	select {
	case <-rec.moved:
	case <-time.After(3 * time.Second):
		t.Fatal("the second move never reached the server")
	}
	waitForMoveSettled(t, w, y)
	select {
	case up := <-rec.uploaded:
		if up != "dst/a.bin" {
			t.Errorf("uploaded %q after move 2, want dst/a.bin", up)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the kept upload never ran after move 2")
	}

	movePlaceholder(t, f, y, x)
	f.markDirty(x)
	rec.mu.Lock()
	before := len(rec.uploads)
	rec.mu.Unlock()
	w.NotifyRenamed(y, x) // move 3: deduped — nothing repoints this one
	w.handleDelete(y)

	modifiedEvent(t, w, `src\a.bin`) // …and the MODIFIED the move delivers

	rec.mu.Lock()
	during := append([]string(nil), rec.uploads[before:]...)
	movesSoFar := len(rec.moves)
	rec.mu.Unlock()
	if len(during) != 0 {
		t.Errorf("uploaded %v while a move still owned the file — the server holds it at dst", during)
	}
	if movesSoFar != 2 {
		t.Errorf("moves before the placement check = %d, want 2", movesSoFar)
	}
	if got := f.inSyncedPaths(); len(got) != 0 {
		t.Errorf("in-sync restored for %v — it is not where the server has it", got)
	}

	select { // the placement check's MOVE
	case <-rec.moved:
	case <-time.After(6 * time.Second):
		t.Fatal("the deduplicated third move never reached the server")
	}
	waitForMoveSettled(t, w, x)
	select { // and the upload finishRename kept, at the NEW server path
	case up := <-rec.uploaded:
		if up != "src/a.bin" {
			t.Errorf("uploaded %q after the placement check, want src/a.bin", up)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the kept upload never ran after the third move")
	}

	rec.mu.Lock()
	moves := append([][2]string(nil), rec.moves...)
	after := append([]string(nil), rec.uploads[before:]...)
	rec.mu.Unlock()
	if len(moves) != 3 {
		t.Fatalf("moves = %v, want 3", moves)
	}
	if moves[2] != [2]string{"dst/a.bin", "src/a.bin"} {
		t.Errorf("third move = %v, want [dst/a.bin src/a.bin]", moves[2])
	}
	for _, up := range after {
		if strings.HasPrefix(up, "dst/") {
			t.Errorf("uploaded %q after the deduplicated move — nothing may land under the old parent", up)
		}
	}
}

// --- fix wave 6: the guard re-arms instead of parking ----------------------

// The state every 0.1.0.28x install can carry: a hydrated placeholder left in
// sync at its own name with the identity of a source the server no longer has
// (the 404 fallback before wave 5 repointed it). The bit clears on the next
// edit, the guard refuses the upload — and before this wave nothing ever
// looked at the file again: reconcile re-scheduled it every pass and the guard
// parked it every pass, with one log line and no error, permanently. The
// deferred placement check is the re-arm: it MOVEs, the MOVE 404s, and the
// fallback uploads the copy at its own name and repoints the identity.
func TestEditOfAPlaceholderWithADeadIdentityIsRepairedAndUploaded(t *testing.T) {
	f := installFakeCf(t)
	shortenRenameVerify(t)
	root := t.TempDir()
	p := filepath.Join(root, "a.bin")
	if err := os.WriteFile(p, []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDirty(p)                // a real edit, waiting to go up
	f.setIdentity(p, "old/a.bin") // on a placeholder naming a source that is gone

	rec := newRecorder()
	rec.moveFails = 99
	rec.moveErr = errors.New(`MOVE "old/a.bin": server returned 404 Not Found: gone`)
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	modifiedEvent(t, w, "a.bin")

	select {
	case up := <-rec.uploaded:
		if up != "a.bin" {
			t.Errorf("uploaded %q, want a.bin", up)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the edit was parked: nothing ever repaired the dead identity")
	}
	// The forced upload's repoint (wave 5) is what finally settles it.
	deadline := time.Now().Add(3 * time.Second)
	for {
		id, ok := f.identityOf(p)
		if ok && id == "a.bin" {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("identity = %q, %v; want \"a.bin\" \u2014 the file is still owed a MOVE nobody can make", id, ok)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // a second upload would land about now
	rec.mu.Lock()
	uploads := append([]string(nil), rec.uploads...)
	moves := append([][2]string(nil), rec.moves...)
	deletes := len(rec.deletes)
	rec.mu.Unlock()
	if len(uploads) != 1 {
		t.Errorf("uploads = %v, want exactly one", uploads)
	}
	if len(moves) != 0 {
		t.Errorf("moves = %v, want none \u2014 every MOVE of a dead source 404s", moves)
	}
	if deletes != 0 {
		t.Errorf("deletes = %d, want 0", deletes)
	}
}

// takeForced consumes the flag at the top of handleChange, so a forced upload
// that does not complete cleanly used to come back UNFORCED — and the identity
// guard then parked the very upload the 404 fallback asked for. Every re-arm
// out of the forced path has to carry the flag with it.
func TestForcedUploadReArmsStayForced(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	doc := filepath.Join(root, "doc.txt")
	if err := os.WriteFile(doc, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDirty(doc)
	f.setIdentity(doc, "old/doc.txt") // foreign throughout, until the repoint

	rec := newRecorder()
	rec.uploadFails = 1
	rec.uploadErr = &os.PathError{Op: "open", Path: doc, Err: windows.ERROR_SHARING_VIOLATION}
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.forceChange(doc) // moveServer's 404 fallback
	select {
	case <-rec.uploaded:
		t.Fatal("the first attempt was supposed to be refused as busy")
	case <-time.After(uploadDebounce + 400*time.Millisecond):
	}

	key := strings.ToLower(doc)
	w.mu.Lock()
	stillForced := w.forced[key]
	_, pending := w.upload[doc]
	w.mu.Unlock()
	if !pending {
		t.Fatal("the busy file was not rescheduled at all")
	}
	if !stillForced {
		t.Error("the re-armed run lost its forced flag \u2014 the guard will park it")
	}

	// Fire the re-arm the way its timer does. (heldRetry is a minute; the
	// delay itself is already pinned by TestSharingViolationRetriesQuietly.)
	w.handleChange(doc)

	rec.mu.Lock()
	uploads := append([]string(nil), rec.uploads...)
	rec.mu.Unlock()
	if len(uploads) != 1 || uploads[0] != "doc.txt" {
		t.Fatalf("uploads = %v, want exactly [doc.txt] \u2014 the retry was parked by the guard", uploads)
	}
	if id, ok := f.identityOf(doc); !ok || id != "doc.txt" {
		t.Errorf("identity = %q, %v; want \"doc.txt\"", id, ok)
	}
}

// Reconcile's in-both heal asks the same question the event path does, so it
// has to refuse the same answer: a placeholder naming another server path is
// owed a MOVE, and stamping it in sync says the opposite. Reachable by moving
// a file over an existing placeholder inside the deduplication window.
func TestReconcileHealSkipsAForeignIdentityAndVerifiesPlacement(t *testing.T) {
	f, rec, w, p := reconcileHealFixture(t, "etag-1", "etag-1")
	defer w.cancel()
	shortenRenameVerify(t)
	f.markNotInSync(p)            // clean, but the bit a move cleared …
	f.setIdentity(p, "old/a.bin") // … and an identity naming somewhere else

	w.Reconcile()

	select {
	case mv := <-rec.moved:
		if mv != [2]string{"old/a.bin", "a.bin"} {
			t.Errorf("move = %v, want [old/a.bin a.bin]", mv)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the heal refused the file and nothing checked where it belongs")
	}
	if got := f.inSyncedPaths(); len(got) != 0 {
		t.Errorf("in-sync restored for %v \u2014 it is not where the server has it", got)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		id, ok := f.identityOf(p)
		if ok && id == "a.bin" {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("identity = %q, %v; want \"a.bin\" after the MOVE", id, ok)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// --- fix wave 7: a directory that was populated EMPTY ----------------------

// The everyday shape of it (VM build 0.1.0.284, /Notes): a directory whose
// first FETCH_PLACEHOLDERS delivered ZERO entries is marked fully populated -
// the transfer passes DISABLE_ON_DEMAND_POPULATION whenever the listing
// succeeded - and the shell never asks about it again. Counting local entries
// then reads it as "lazy, nothing to do", so anything the server gains inside
// it afterwards has nobody left to fetch it: not on this pass, not on any
// pass, ever. The directory's own attribute is what tells the two apart.
func TestReconcilePullsIntoAPopulatedEmptyDirectory(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	notes := filepath.Join(root, "Notes")
	if err := os.MkdirAll(notes, 0o755); err != nil {
		t.Fatal(err)
	}
	f.markPopulated(notes) // its transfer completed, with zero entries

	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("Notes", true, "etag-notes", "")}
	rec.listing["Notes"] = []cfapi.PlaceholderInfo{ph("subX", true, "etag-subx", "")}
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	rec.mu.Lock()
	listed := rec.listCalls["Notes"]
	rec.mu.Unlock()
	if listed == 0 {
		t.Fatal("the populated-empty directory was never listed - the server's addition can never arrive")
	}
	if got := f.createdNames(); len(got) != 1 || got[0] != "subX" {
		t.Errorf("created %v, want exactly [subX]", got)
	}
}

// The other half of the same question: a directory nobody has populated yet
// must still be left alone. The shell issues its FETCH_PLACEHOLDERS on the
// first open, and pulling its children here would race that transfer.
func TestReconcileSkipsANeverPopulatedDirectory(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	notes := filepath.Join(root, "Notes")
	if err := os.MkdirAll(notes, 0o755); err != nil {
		t.Fatal(err)
	}
	// deliberately NOT markPopulated: a freshly created lazy placeholder

	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("Notes", true, "etag-notes", "")}
	rec.listing["Notes"] = []cfapi.PlaceholderInfo{ph("subX", true, "etag-subx", "")}
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	rec.mu.Lock()
	listed := rec.listCalls["Notes"]
	rec.mu.Unlock()
	if listed != 0 {
		t.Errorf("listed the lazy directory %d time(s) - that races the shell's own first fetch", listed)
	}
	if got := f.createdNames(); len(got) != 0 {
		t.Errorf("created %v inside a directory the shell has not populated yet", got)
	}
}

// A PLAIN empty directory in-both is a flatten victim, and the heal that
// replaces it with a placeholder lists it and populates it from the server.
// That path must keep working: a plain directory has no population state to
// read, so it is always "populated" and nothing may start skipping it.
func TestReconcileStillPopulatesAPlainEmptyDirectory(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	f.markPlain(sub) // lost its cloud state (Deck #580)

	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("sub", true, "etag-sub", "")}
	rec.listing["sub"] = []cfapi.PlaceholderInfo{ph("child.txt", false, "etag-child", "fid-child")}
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	rec.mu.Lock()
	listed := rec.listCalls["sub"]
	rec.mu.Unlock()
	if listed == 0 {
		t.Fatal("the plain empty directory was never listed")
	}
	got := f.createdNames()
	var sawChild bool
	for _, n := range got {
		if n == "child.txt" {
			sawChild = true
		}
	}
	if !sawChild {
		t.Errorf("created %v, want the server's child.txt among them", got)
	}
}

// --- fix wave 7: the first pass runs at start ------------------------------

// pollLoop waits a whole poll interval (30 s, or five minutes on push-driven
// installs) before its first pass, and nothing else poked the watcher at mount
// time - so after every start the shell's own root fetch won the race, and for
// that whole interval no reconcile had run at all. Anything added on another
// device while Nimbo was off waited for it.
func TestWatcherReconcilesOnceAtStart(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	rec := newRecorder()
	rec.listing[""] = nil
	w, err := New(context.Background(), root, "", time.Hour, rec.ops())
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	deadline := time.Now().Add(4 * time.Second)
	for {
		rec.mu.Lock()
		listed := rec.listCalls[""]
		rec.mu.Unlock()
		if listed > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no reconcile pass ran after start - the first one waits for the poll tick")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// --- fix wave 8: the check waits for the parent's move, and forced re-arms --

// A file saved INSIDE a folder whose server MOVE is still running carries an
// identity naming the old parent — which reads exactly like a file owed a move
// of its own. Moving it would put it at a path the parent's MOVE is about to
// take with Overwrite: T, burying the edit in the server's trash (or making
// the parent's failure look applied, because the destination collection our
// MOVE pre-created is then there for the lost-response Stat to find). The
// parent's own repointTree stamps the child correctly before the parent's mark
// clears, so waiting is always the right answer.
func TestVerifyPlacementWaitsForTheParentsMove(t *testing.T) {
	f := installFakeCf(t)
	shortenRenameVerify(t)
	root := t.TempDir()
	newDir := filepath.Join(root, "new", "dir")
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(newDir, "child.bin")
	if err := os.WriteFile(child, []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The user moved old/dir to new/dir; the server still has both under the
	// old name, and the child was edited while the MOVE was in flight.
	f.setIdentity(newDir, "old/dir")
	f.setIdentity(child, "old/dir/child.bin")
	f.markDirty(child)

	rec := newRecorder()
	rec.moveGate = make(chan struct{}) // the parent's MOVE, still running
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	oldDir := filepath.Join(root, "old", "dir")
	w.beginRename(oldDir, newDir)
	go w.handleRename(oldDir, newDir)
	waitForMoveInFlight(t, w, newDir)

	modifiedEvent(t, w, `new\dir\child.bin`) // the edit's own event

	rec.mu.Lock()
	uploadsDuring := len(rec.uploads)
	rec.mu.Unlock()
	if uploadsDuring != 0 {
		t.Errorf("uploaded %d thing(s) while the parent's MOVE was still running", uploadsDuring)
	}

	close(rec.moveGate)
	select {
	case mv := <-rec.moved:
		if mv != [2]string{"old/dir", "new/dir"} {
			t.Errorf("move = %v, want [old/dir new/dir]", mv)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the parent's move never completed")
	}
	select { // repointTree keeps the child's pending upload and sends it
	case up := <-rec.uploaded:
		if up != "new/dir/child.bin" {
			t.Errorf("uploaded %q, want new/dir/child.bin", up)
		}
	// Generous: the edit now waits out the parent's move at moveWaitRetry
	// polls (2 s each) before its upload, so the honest margin here is a few
	// polls, not one — a tight wait only ever turns a slow CI box into a
	// failure. A blocked upload never returns, so nothing hides behind it.
	case <-time.After(10 * time.Second):
		t.Fatal("the child's edit never reached the server")
	}

	time.Sleep(2*renameVerifyDelay + renameVerifyPoll + 300*time.Millisecond)
	rec.mu.Lock()
	moves := append([][2]string(nil), rec.moves...)
	rec.mu.Unlock()
	if len(moves) != 1 {
		t.Errorf("moves = %v, want exactly one (the parent's) — the child was moved out from under it", moves)
	}
}

// cancelInflight drops a doomed upload's timer and context, and must drop its
// FORCED flag with them: a forced flag now sits behind a held/busy/backoff
// window, and one left on a vacated path makes the NEXT occupant of that path
// upload with the gate and the identity guard skipped.
func TestCancelInflightDropsTheForcedFlag(t *testing.T) {
	f := installFakeCf(t)
	setRenameVerifyDelay(t, 10*time.Second) // the check must not act inside this test
	root := t.TempDir()
	p := filepath.Join(root, "doc.txt")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDirty(p)

	rec := newRecorder()
	rec.uploadFails = 1
	rec.uploadErr = &os.PathError{Op: "open", Path: p, Err: windows.ERROR_SHARING_VIOLATION}
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.forceChange(p) // the 404 fallback's upload …
	time.Sleep(uploadDebounce + 400*time.Millisecond)
	// … refused as busy, so the flag is waiting behind a 60s re-arm when the
	// user renames the file away.
	q := filepath.Join(root, "moved.txt")
	if err := os.Rename(p, q); err != nil {
		t.Fatal(err)
	}
	w.handleRename(p, q)

	w.mu.Lock()
	leftover := w.forced[strings.ToLower(p)]
	w.mu.Unlock()
	if leftover {
		t.Error("a forced flag was left on the vacated path")
	}

	// A different file then arrives at that path, itself owed a move. It must
	// be judged on its own terms — the guard, not the leftover flag.
	if err := os.WriteFile(p, []byte("someone else's file"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDirty(p)
	f.setIdentity(p, "old/doc.txt")
	rec.mu.Lock()
	before := len(rec.uploads)
	rec.mu.Unlock()

	modifiedEvent(t, w, "doc.txt")

	rec.mu.Lock()
	after := append([]string(nil), rec.uploads[before:]...)
	rec.mu.Unlock()
	if len(after) != 0 {
		t.Errorf("uploaded %v — the new occupant was pushed as though it were the forced one", after)
	}
}

// A forced MKCOL that fails must be retried FORCED: the retry sees a directory
// placeholder (nothing to upload) and no-ops at the gate otherwise, so the
// folder the 404 fallback asked for waits for the next full sweep while every
// upload into it 409s.
func TestForcedMkdirReArmStaysForced(t *testing.T) {
	ob, om := retryBase, retryMax
	retryBase, retryMax = 20*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { retryBase, retryMax = ob, om })

	installFakeCf(t)
	root := t.TempDir()
	dir := filepath.Join(root, "sub")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	rec := newRecorder()
	rec.mkdirFails = 1
	rec.mkdirErr = errors.New("MKCOL: server returned 503 Service Unavailable")
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.forceChange(dir)

	deadline := time.Now().Add(3 * time.Second)
	for {
		rec.mu.Lock()
		mkdirs := append([]string(nil), rec.mkdirs...)
		rec.mu.Unlock()
		if len(mkdirs) > 0 {
			if len(mkdirs) != 1 || mkdirs[0] != "sub" {
				t.Errorf("MKCOLs = %v, want exactly [sub]", mkdirs)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the failed forced MKCOL was never retried — the retry lost its forced flag")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// "is missing on the server and was never downloaded" is the truth when the
// file really is gone. It is NOT the truth when only the SOURCE of a repair
// MOVE is gone and the server holds the destination — a dead identity on a
// file that is in both places. Repoint it and say nothing.
func TestMove404WithTheDestinationOnTheServerIsSilent(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	newp := filepath.Join(root, "stub.txt")
	if err := os.WriteFile(newp, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDehydrated(newp)

	rec := newRecorder()
	rec.moveFails = 99
	rec.moveErr = errors.New(`MOVE "old/stub.txt": server returned 404 Not Found: gone`)
	rec.statExists = map[string]bool{"stub.txt": true} // the server HAS it here
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.moveServer("old/stub.txt", newp, false)

	rec.mu.Lock()
	var reported []reportRec
	for _, r := range rec.reports {
		if r.err != nil {
			reported = append(reported, r)
		}
	}
	rec.mu.Unlock()
	if len(reported) != 0 {
		t.Errorf("reported %v — the file is not missing, only its old name is", reported)
	}
	if id, ok := f.identityOf(newp); !ok || id != "stub.txt" {
		t.Errorf("identity = %q, %v; want repointed to \"stub.txt\"", id, ok)
	}
}

// baselineEventually polls the recorder until remote carries want (finishRename
// runs after the MOVE the rec.moved channel announces, so an immediate read
// would race it).
func baselineEventually(t *testing.T, rec *recorder, remote, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		rec.mu.Lock()
		got, ok := rec.baselines[remote]
		rec.mu.Unlock()
		if ok && got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("baseline for %q = %q, %v; want %q", remote, got, ok, want)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestMoveCarriesTheConflictBaseline(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	newp := filepath.Join(root, "sub", "a.bin")
	if err := os.WriteFile(newp, []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.setIdentity(newp, "a.bin") // moved on disk; the server still knows it as a.bin
	f.markNotInSync(newp)
	rec := newRecorder()
	rec.baselines["a.bin"] = "etag-same"
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleRename(filepath.Join(root, "a.bin"), newp)

	select {
	case <-rec.moved:
	case <-time.After(3 * time.Second):
		t.Fatal("the move never reached the server")
	}
	baselineEventually(t, rec, "sub/a.bin", "etag-same")
	rec.mu.Lock()
	_, stale := rec.baselines["a.bin"]
	rec.mu.Unlock()
	if stale {
		t.Error("the baseline of the vacated server path was kept — a later file created there would inherit it")
	}
}

func TestDirectoryMoveCarriesDescendantBaselines(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	newDir := filepath.Join(root, "e")
	if err := os.MkdirAll(filepath.Join(newDir, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(newDir, "x.txt")
	deep := filepath.Join(newDir, "inner", "y.txt")
	for _, p := range []string{child, deep} {
		if err := os.WriteFile(p, []byte("bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.setIdentity(newDir, "d")
	f.setIdentity(filepath.Join(newDir, "inner"), "d/inner")
	f.setIdentity(child, "d/x.txt")
	f.setIdentity(deep, "d/inner/y.txt")
	rec := newRecorder()
	rec.baselines["d"] = "e-dir"
	rec.baselines["d/inner"] = "e-inner"
	rec.baselines["d/x.txt"] = "e-x"
	rec.baselines["d/inner/y.txt"] = "e-y"
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleRename(filepath.Join(root, "d"), newDir)

	select {
	case <-rec.moved:
	case <-time.After(3 * time.Second):
		t.Fatal("the move never reached the server")
	}
	baselineEventually(t, rec, "e", "e-dir")
	baselineEventually(t, rec, "e/inner", "e-inner")
	baselineEventually(t, rec, "e/x.txt", "e-x")
	baselineEventually(t, rec, "e/inner/y.txt", "e-y")
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, old := range []string{"d", "d/inner", "d/x.txt", "d/inner/y.txt"} {
		if _, ok := rec.baselines[old]; ok {
			t.Errorf("baseline for the vacated %q was kept", old)
		}
	}
}

// The baseline store persists its WHOLE file on every call, so the carry of a
// moved subtree has to arrive as ONE call. Per-descendant carries made a
// 1,000-file folder move 2,000 full rewrites of a multi-megabyte JSON — run
// synchronously inside finishRename while the move's in-flight mark is held,
// which is exactly the window verifyPlacement's callers are waiting on.
func TestDirectoryMoveCarriesDescendantBaselinesInOneWrite(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	newDir := filepath.Join(root, "e")
	if err := os.MkdirAll(filepath.Join(newDir, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(newDir, "x.txt")
	deep := filepath.Join(newDir, "inner", "y.txt")
	for _, p := range []string{child, deep} {
		if err := os.WriteFile(p, []byte("bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.setIdentity(newDir, "d")
	f.setIdentity(filepath.Join(newDir, "inner"), "d/inner")
	f.setIdentity(child, "d/x.txt")
	f.setIdentity(deep, "d/inner/y.txt")
	rec := newRecorder()
	rec.baselines["d"] = "e-dir"
	rec.baselines["d/inner"] = "e-inner"
	rec.baselines["d/x.txt"] = "e-x"
	rec.baselines["d/inner/y.txt"] = "e-y"
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleRename(filepath.Join(root, "d"), newDir)

	select {
	case <-rec.moved:
	case <-time.After(3 * time.Second):
		t.Fatal("the move never reached the server")
	}
	baselineEventually(t, rec, "e/inner/y.txt", "e-y") // the last write of the walk

	batches := rec.baselineBatches()
	if len(batches) != 1 {
		t.Fatalf("%d batched carries, want exactly 1 (one persist for the whole subtree): %v", len(batches), batches)
	}
	want := map[string]string{
		"d": "e", "d/inner": "e/inner", "d/x.txt": "e/x.txt", "d/inner/y.txt": "e/inner/y.txt",
	}
	if len(batches[0]) != len(want) {
		t.Errorf("the carry named %d paths, want %d (the moved directory and every descendant): %v",
			len(batches[0]), len(want), batches[0])
	}
	for _, p := range batches[0] {
		if want[p[0]] != p[1] {
			t.Errorf("pair %v is not a move of a known path", p)
		}
		delete(want, p[0])
	}
	for src := range want {
		t.Errorf("no pair carried %q", src)
	}
	rec.mu.Lock()
	records, forgets := rec.singleRecords, rec.singleForgets
	rec.mu.Unlock()
	if records != 0 || forgets != 0 {
		t.Errorf("one-item baseline writes: %d records, %d forgets — every one is a whole-file rewrite", records, forgets)
	}
}

// Reconcile's pull has the same shape: one baseline per created item was one
// whole-file rewrite per item, so a 1,000-item directory pull was 1,000.
func TestReconcilePullRecordsBaselinesInOneWrite(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{
		ph("a.txt", false, "e-a", "f-a"),
		ph("b.txt", false, "e-b", "f-b"),
		ph("c.txt", false, "e-c", "f-c"),
	}
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	rec.mu.Lock()
	batches := append([]map[string]string(nil), rec.recordBatches...)
	records := rec.singleRecords
	rec.mu.Unlock()
	if len(batches) != 1 {
		t.Fatalf("%d batched baseline writes, want exactly 1 for the pull: %v", len(batches), batches)
	}
	for name, etag := range map[string]string{"a.txt": "e-a", "b.txt": "e-b", "c.txt": "e-c"} {
		if batches[0][name] != etag {
			t.Errorf("batch[%q] = %q, want %q", name, batches[0][name], etag)
		}
	}
	if records != 0 {
		t.Errorf("%d one-item baseline writes during the pull — each is a whole-file rewrite", records)
	}
}

// M9 (c): the upload a move KEEPS (an edit made just before the rename) must
// see the baseline under its NEW name when it runs, or the conflict check
// finds none and parks the server's own untouched copy as a "conflicted
// copy". The batched carry must therefore land BEFORE the kept upload is
// scheduled, not after the walk that follows it.
func TestKeptUploadAfterAMoveSeesTheCarriedBaseline(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	newp := filepath.Join(root, "sub", "a.bin")
	if err := os.WriteFile(newp, []byte("edited bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.setIdentity(newp, "a.bin")
	f.markDirty(newp) // edited, then moved: the upload is kept and re-sent
	rec := newRecorder()
	rec.baselines["a.bin"] = "etag-same"
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleRename(filepath.Join(root, "a.bin"), newp)

	select {
	case <-rec.moved:
	case <-time.After(3 * time.Second):
		t.Fatal("the move never reached the server")
	}
	select {
	case up := <-rec.uploaded:
		if up != "sub/a.bin" {
			t.Fatalf("uploaded %q, want sub/a.bin", up)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the kept upload never ran")
	}
	rec.mu.Lock()
	saw := rec.uploadSawBase["sub/a.bin"]
	rec.mu.Unlock()
	if saw != "etag-same" {
		t.Errorf("the kept upload saw baseline %q for sub/a.bin, want etag-same — with none, an unchanged server copy is parked as a conflicted copy", saw)
	}
}

// The dead-identity population: a file the server holds at THIS name, in
// sync and clean locally, whose placeholder still names a server path that no
// longer exists (every hydrated 404 fallback before fix wave 5 made one, and
// every "free up space" since turned it into a stub that 404s on open). The
// first pass after start repoints it in place — no MOVE, no upload.
func TestReconcileRepointsAnInSyncDeadIdentity(t *testing.T) {
	f, rec, w, p := reconcileHealFixture(t, "etag-1", "etag-1")
	defer w.cancel()
	f.setIdentity(p, "old/a.bin")

	w.Reconcile()
	time.Sleep(uploadDebounce + 400*time.Millisecond)

	if id, ok := f.identityOf(p); !ok || id != "a.bin" {
		t.Errorf("identity = %q, %v; want \"a.bin\" — the next open would hydrate from a dead path", id, ok)
	}
	rec.mu.Lock()
	moves, uploads := len(rec.moves), append([]string(nil), rec.uploads...)
	rec.mu.Unlock()
	if moves != 0 {
		t.Errorf("issued %d MOVE(s) — the server already holds the file here; a MOVE from the dead source can only 404 or bury a duplicate", moves)
	}
	if len(uploads) != 0 {
		t.Errorf("uploaded %v — the file is in sync", uploads)
	}
}

func TestReconcileRepointsAnInSyncDeadIdentityOnAStub(t *testing.T) {
	f, rec, w, p := reconcileHealFixture(t, "etag-1", "etag-1")
	defer w.cancel()
	f.markDehydrated(p)
	f.setIdentity(p, "old/a.bin")

	w.Reconcile()
	time.Sleep(uploadDebounce + 400*time.Millisecond)

	if id, ok := f.identityOf(p); !ok || id != "a.bin" {
		t.Errorf("identity = %q, %v; want \"a.bin\"", id, ok)
	}
	rec.mu.Lock()
	moves, uploads := len(rec.moves), len(rec.uploads)
	rec.mu.Unlock()
	if moves != 0 || uploads != 0 {
		t.Errorf("moves=%d uploads=%d, want none — a stub is repointed without touching its data", moves, uploads)
	}
}

func TestReconcileLeavesAMatchingIdentityAlone(t *testing.T) {
	f, _, w, p := reconcileHealFixture(t, "etag-1", "etag-1")
	defer w.cancel()

	w.Reconcile()

	if got := f.markInSyncRepoints(); len(got) != 0 {
		t.Errorf("repointed %v — the identity already named the server path", got)
	}
	if id, ok := f.identityOf(p); !ok || id != "a.bin" {
		t.Errorf("identity = %q, %v; want \"a.bin\" unchanged", id, ok)
	}
}

// The repair is priced for the first pass after start only: a steady-state
// poll must not pay one identity read per in-sync file.
func TestReconcileRepointsDeadIdentitiesOnlyOnAFullSweep(t *testing.T) {
	f, _, w, p := reconcileHealFixture(t, "etag-1", "etag-1")
	defer w.cancel()

	w.Reconcile() // the first pass: a full sweep, nothing to repair yet
	f.setIdentity(p, "old/a.bin")
	w.Reconcile() // a steady-state pass

	if id, _ := f.identityOf(p); id != "old/a.bin" {
		t.Errorf("identity = %q — a steady-state pass repointed it; that read is reserved for the full sweep", id)
	}
	w.noteEventLoss() // lost events force the next pass skip-free
	w.Reconcile()
	if id, _ := f.identityOf(p); id != "a.bin" {
		t.Errorf("identity = %q, want \"a.bin\" after a skip-free pass", id)
	}
}

// An entry Windows refuses to open with ERROR_CLOUD_FILE_METADATA_CORRUPT is
// a permanent Windows fault reconcile can do nothing about; the user must at
// least be told, once, instead of reading "Up to date" over a file they cannot
// open.
func TestReconcileReportsACorruptEntryOnce(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	bad := filepath.Join(root, "bad.bin")
	if err := os.WriteFile(bad, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f.markCorrupt(bad)
	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{
		{Name: "bad.bin", Size: 4096, ModTime: time.Now(), Identity: []byte("bad.bin"), ETag: "e1"},
	}
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()
	// A second FULL sweep, or the dedupe is not being tested at all: a
	// steady-state pass never reads an in-sync file's identity, so "once"
	// would hold with no memory of the first report whatsoever.
	w.noteEventLoss()
	w.Reconcile()

	rec.mu.Lock()
	var corrupt []reportRec
	for _, r := range rec.reports {
		if r.kind == "corrupt" {
			corrupt = append(corrupt, r)
		}
	}
	deletes := len(rec.deletes)
	rec.mu.Unlock()
	if len(corrupt) != 1 || corrupt[0].path != "bad.bin" || corrupt[0].err == nil {
		t.Fatalf("corrupt reports = %+v, want exactly one for bad.bin with an error", corrupt)
	}
	if deletes != 0 {
		t.Errorf("issued %d server DELETE(s) for an entry that only Windows cannot read", deletes)
	}
	if _, err := os.Lstat(bad); err != nil {
		t.Error("the local entry was removed — it cannot be, and it holds the user's only marker of the fault")
	}
}

// The report has to be self-contained. It used to end at "see TROUBLESHOOTING",
// a document that is origin-only — excluded from every public snapshot — so
// the one user who ever saw this was pointed at a page they cannot reach.
// Nothing about this fault is guessable: it needs an elevated shell, a filter
// detach, and a reparse-point delete before the entry will go.
func TestTheCorruptReportCarriesItsOwnRecoverySteps(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	bad := filepath.Join(root, "bad.bin")
	if err := os.WriteFile(bad, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f.markCorrupt(bad)
	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{
		{Name: "bad.bin", Size: 4096, ModTime: time.Now(), Identity: []byte("bad.bin"), ETag: "e1"},
	}
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	rec.mu.Lock()
	var msg string
	for _, r := range rec.reports {
		if r.kind == "corrupt" && r.err != nil {
			msg = r.err.Error()
		}
	}
	rec.mu.Unlock()
	if msg == "" {
		t.Fatal("no corrupt report with an error")
	}
	for _, want := range []string{
		"bad.bin", // which entry
		bad,       // the full local path, ready to paste
		"fltmc detach cldflt " + filepath.VolumeName(bad), // the real drive, not "C:"
		"fsutil reparsepoint delete",                      // the measured step before the delete
		"Remove-Item",
		"fltmc attach cldflt " + filepath.VolumeName(bad),
		"OneDrive", // the detach pauses every provider on that drive
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the report does not carry %q:\n%s", want, msg)
		}
	}
	if strings.Contains(strings.ToLower(msg), "troubleshooting") {
		t.Errorf("the report points at a document the user cannot reach:\n%s", msg)
	}
	if rec.logged("TROUBLESHOOTING") {
		t.Errorf("the log still points at TROUBLESHOOTING: %v", rec.logLines())
	}
}

// A LOCAL-ONLY corrupt entry (the server listing does not have it) fell
// through to os.RemoveAll, which fails with 363 too — so every pass logged a
// remove failure and withheld the parent's ETag, re-listing that directory
// for as long as the entry existed. The entry is permanent: withholding the
// ETag forever buys nothing.
func TestReconcileLeavesALocalOnlyCorruptEntryAlone(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(sub, "bad.bin")
	if err := os.WriteFile(bad, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f.markCorrupt(bad)
	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("sub", true, "etag-sub", "")}
	rec.listing["sub"] = []cfapi.PlaceholderInfo{} // the server does not have bad.bin
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()

	rec.mu.Lock()
	var corrupt int
	for _, r := range rec.reports {
		if r.kind == "corrupt" {
			corrupt++
		}
	}
	base := rec.baselines["sub"]
	rec.mu.Unlock()
	if corrupt != 1 {
		t.Errorf("%d corrupt reports, want 1", corrupt)
	}
	// Neither outcome of a removal attempt may appear: on the real system
	// RemoveAll fails with 363 ("reconcile remove"), and in the fake world,
	// where only the identity read is broken, it would succeed ("pulled
	// delete") and take the entry with it.
	for _, line := range []string{"reconcile remove", "pulled delete"} {
		if rec.logged(line) {
			t.Errorf("the entry was handed to RemoveAll (%q): %v", line, rec.logLines())
		}
	}
	if base != "etag-sub" {
		t.Errorf("baseline for sub = %q, want etag-sub — a permanently broken entry must not withhold its parent's ETag on every pass", base)
	}
	if _, err := os.Lstat(bad); err != nil {
		t.Error("the local entry was removed")
	}
}

// M8 (d): the in-sync repoint owns ONLY in-sync files. A NOT-in-sync
// placeholder with a foreign identity is a move's business — the heal above it
// hands it to the placement check — and repointing it here would say the
// opposite of what that check is about to decide.
func TestReconcileDoesNotRepointANotInSyncDeadIdentity(t *testing.T) {
	f, rec, w, p := reconcileHealFixture(t, "etag-1", "etag-1")
	defer w.cancel()
	f.markNotInSync(p) // a move cleared the bit
	f.setIdentity(p, "old/a.bin")
	rec.statExists = map[string]bool{} // the placement check finds nothing to move

	w.Reconcile()
	time.Sleep(300 * time.Millisecond) // let the placement check run

	if rec.logged("vfs repointed ") {
		t.Errorf("the in-sync repoint claimed a not-in-sync file: %v", rec.logLines())
	}
	if !rec.logged("a move owns it; checking where it belongs") {
		t.Errorf("the file was not handed to the placement check: %v", rec.logLines())
	}
}

// M8 (e): a move in flight ABOVE the file (its parent directory is being
// moved on the server) means every identity under it still names the old
// parent and is about to be repointed by the move itself. Repointing one
// here, from a listing taken before the move, races that walk — so the pass
// leaves it alone and, crucially, does NOT record the directory's ETag, or
// the subtree skip would hide the drift until the next restart.
func TestReconcileSkipsAnInSyncDeadIdentityUnderAMoveInFlight(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	dir := filepath.Join(root, "b")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(dir, "a.bin")
	if err := os.WriteFile(child, []byte("server bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.setIdentity(dir, "b")
	f.setIdentity(child, "old/a.bin") // still names the pre-move parent
	rec := newRecorder()
	rec.moveGate = make(chan struct{})
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("b", true, "etag-b", "")}
	rec.listing["b"] = []cfapi.PlaceholderInfo{
		{Name: "a.bin", Size: 12, ModTime: time.Now(), Identity: []byte("b/a.bin"), ETag: "etag-a"},
	}
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	go w.handleRename(filepath.Join(root, "a"), dir) // the parent's MOVE, held by the gate
	deadline := time.Now().Add(3 * time.Second)
	for !w.moveInFlight(dir) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !w.moveInFlight(dir) {
		t.Fatal("the parent's move never went in flight")
	}

	w.Reconcile()

	if id, _ := f.identityOf(child); id != "old/a.bin" {
		t.Errorf("identity = %q — the pass repointed a file whose parent's MOVE is still running", id)
	}
	rec.mu.Lock()
	_, recorded := rec.baselines["b"]
	rec.mu.Unlock()
	if recorded {
		t.Error("the directory's ETag was recorded although an item in it was skipped — the subtree skip will hide it until the next restart")
	}
	close(rec.moveGate)
	select {
	case <-rec.moved:
	case <-time.After(3 * time.Second):
		t.Fatal("the parent's move never completed")
	}
}

// Reconcile's third rename detector (a local-only placeholder whose identity
// names another server path) reads the same evidence a live move does, so it
// has to stand down while a move is running on the path OR ON ANY DIRECTORY
// ABOVE IT. With the single-key check, a file inside a directory whose MOVE
// was still running got a second MOVE of its own — to the very path the
// parent's move is about to take, with Overwrite: T.
func TestReconcileThirdDetectorStandsDownUnderAMoveInFlight(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	dir := filepath.Join(root, "b")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(dir, "a.bin")
	if err := os.WriteFile(child, []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.setIdentity(dir, "b")
	f.setIdentity(child, "old/a.bin") // the server has it under the old parent
	rec := newRecorder()
	rec.moveGate = make(chan struct{})
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("b", true, "etag-b", "")}
	rec.listing["b"] = []cfapi.PlaceholderInfo{} // not in the listing: the local-only branch
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	go w.handleRename(filepath.Join(root, "a"), dir)
	deadline := time.Now().Add(3 * time.Second)
	for !w.moveInFlight(dir) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !w.moveInFlight(dir) {
		t.Fatal("the parent's move never went in flight")
	}

	done := make(chan struct{})
	go func() {
		w.Reconcile()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		// Generous on purpose: the pass is milliseconds of work, and a margin
		// this wide only ever delays a real failure — a blocked pass never
		// returns at all, because the MOVE it issued is waiting on the gate.
		close(rec.moveGate) // let the parent's MOVE finish so the run can end
		t.Fatal("Reconcile blocked behind the in-flight move — it issued a second MOVE for a file under it")
	}

	rec.mu.Lock()
	moves := len(rec.moves)
	_, recorded := rec.baselines["b"]
	rec.mu.Unlock()
	if moves != 0 {
		t.Errorf("%d MOVE(s) completed during the parent's move — the child was moved into the path the parent's MOVE is about to take", moves)
	}
	if recorded {
		t.Error("the directory's ETag was recorded although an item in it was skipped")
	}
	close(rec.moveGate)
	select {
	case <-rec.moved:
	case <-time.After(3 * time.Second):
		t.Fatal("the parent's move never completed")
	}
	time.Sleep(300 * time.Millisecond) // a second MOVE would land about now
	rec.mu.Lock()
	moves = len(rec.moves)
	rec.mu.Unlock()
	if moves != 1 {
		t.Errorf("MOVE count = %d, want 1 (the parent's own)", moves)
	}
}

// I7. The baseline carry of a moved subtree lands in ONE write at the end of
// the repoint walk, so for the length of that walk a descendant can be in a
// state nothing else refuses: its identity already names the NEW path (so the
// foreign-identity guard has nothing to say) while its baseline is still
// recorded under the OLD one. A live edit in that window — the user, or a GIS
// scratch/lock file, saving inside the folder they just moved — used to upload
// straight away, and the conflict check then read "no baseline recorded" over
// a server copy the MOVE had just placed: the server's own version parked as a
// spurious "conflicted copy". That is the very symptom the carry was shipped
// to fix, on the very operation it was shipped for.
//
// Waiting is always right, as moveInFlightOrAbove already argues for the
// placement check: the move lands, the carry with it, and the upload then runs
// with the truth in hand.
//
// A FORCED upload is the sharper case — it is exempt from the identity guard
// by design (moveServer's 404 fallback) — and it is the one that can be shown
// end to end: the walk really carries this descendant's baseline, and the
// upload really reads it at the new name.
func TestAForcedUploadInsideAMovedFolderWaitsForTheCarriedBaseline(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	dir := filepath.Join(root, "b") // the server still knows this folder as "a"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(dir, "x.txt")
	if err := os.WriteFile(child, []byte("edited bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.setIdentity(dir, "a")
	f.setIdentity(child, "a/x.txt") // the walk has not reached it yet
	f.markDirty(child)              // a real local edit is owed
	rec := newRecorder()
	rec.moveGate = make(chan struct{})
	rec.baselines["a/x.txt"] = "etag-1" // recorded under the PRE-move name
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	go w.handleRename(filepath.Join(root, "a"), dir)
	deadline := time.Now().Add(3 * time.Second)
	for !w.moveInFlight(dir) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !w.moveInFlight(dir) {
		t.Fatal("the parent's move never went in flight")
	}

	w.forceChange(child) // the 404-fallback flag: exempt from the identity guard
	w.handleChange(child)

	select {
	case up := <-rec.uploaded:
		t.Fatalf("uploaded %q while the parent's MOVE was still in flight — its baseline has not been carried yet, so the conflict check parks the server's copy", up)
	case <-time.After(1500 * time.Millisecond):
	}

	close(rec.moveGate)
	select {
	case <-rec.moved:
	case <-time.After(3 * time.Second):
		t.Fatal("the parent's move never completed")
	}
	select {
	case up := <-rec.uploaded:
		if up != "b/x.txt" {
			t.Errorf("uploaded %q, want b/x.txt — the new server path", up)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the edit never uploaded after the move landed — the wait must re-arm, not drop it")
	}
	rec.mu.Lock()
	uploads := append([]string(nil), rec.uploads...)
	saw := rec.uploadSawBase["b/x.txt"]
	rec.mu.Unlock()
	if len(uploads) != 1 {
		t.Errorf("uploads = %v, want exactly one", uploads)
	}
	if saw != "etag-1" {
		t.Errorf("the upload saw baseline %q for b/x.txt, want etag-1 (the baseline the move carried); with none, the server's own copy is parked as a conflicted copy", saw)
	}
}

// The review's own shape: an ordinary (unforced) edit on a descendant the walk
// has ALREADY repointed — identity == the new server path, so the
// foreign-identity guard does not fire — while the parent's MOVE is still
// running.
func TestAnEditOnAnAlreadyRepointedDescendantWaitsForTheMove(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	dir := filepath.Join(root, "b")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(dir, "x.txt")
	if err := os.WriteFile(child, []byte("edited bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.setIdentity(dir, "a")
	f.setIdentity(child, "b/x.txt") // the walk repointed it; the carry has not landed
	f.markDirty(child)
	rec := newRecorder()
	rec.moveGate = make(chan struct{})
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	go w.handleRename(filepath.Join(root, "a"), dir)
	deadline := time.Now().Add(3 * time.Second)
	for !w.moveInFlight(dir) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !w.moveInFlight(dir) {
		t.Fatal("the parent's move never went in flight")
	}

	w.handleChange(child)

	select {
	case up := <-rec.uploaded:
		t.Fatalf("uploaded %q while the parent's MOVE was still in flight", up)
	case <-time.After(1500 * time.Millisecond):
	}

	close(rec.moveGate)
	select {
	case <-rec.moved:
	case <-time.After(3 * time.Second):
		t.Fatal("the parent's move never completed")
	}
	select {
	case up := <-rec.uploaded:
		if up != "b/x.txt" {
			t.Errorf("uploaded %q, want b/x.txt", up)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the edit never uploaded after the move landed")
	}
	rec.mu.Lock()
	uploads := append([]string(nil), rec.uploads...)
	rec.mu.Unlock()
	if len(uploads) != 1 {
		t.Errorf("uploads = %v, want exactly one", uploads)
	}
}

// The recovery commands exist to be pasted. Both paths are SINGLE-quoted with
// any apostrophe doubled (PowerShell's own escape): double quotes would expand
// $ and backticks, and apostrophes in folder names are common. No product name
// appears either — white-label builds run this code.
func TestTheCorruptRecoveryQuotesPathsForPasting(t *testing.T) {
	const full = `C:\Users\Bob\Nextcloud\Bob's GIS\a$b`
	got := corruptRecovery(full)
	for _, want := range []string{
		`fsutil reparsepoint delete 'C:\Users\Bob\Nextcloud\Bob''s GIS\a$b'`,
		`Remove-Item -LiteralPath '\\?\C:\Users\Bob\Nextcloud\Bob''s GIS\a$b' -Recurse -Force`,
		"fltmc detach cldflt C:",
		"fltmc attach cldflt C:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the recovery does not carry\n  %s\ngot:\n  %s", want, got)
		}
	}
	if strings.Contains(got, `"`) {
		t.Errorf("a double-quoted path expands $ and backticks:\n%s", got)
	}
	if strings.Contains(got, "Nimbo") {
		t.Errorf("the text names the product, which a white-label build is not:\n%s", got)
	}
}

// The sync ROOT has no remote path of its own, so the report used to carry ""
// — which every renderer turns into ".", including the toast's
// filepath.Base. Name it the way the user sees it.
func TestACorruptSyncRootIsNamedNotRenderedAsADot(t *testing.T) {
	root := t.TempDir()
	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	err := &os.PathError{Op: "FindFirstFile", Path: root, Err: windows.ERROR_CLOUD_FILE_METADATA_CORRUPT}
	if !w.noteCorrupt(root, err) {
		t.Fatal("the 363 fault on the sync root was not recognised")
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.reports) != 1 || rec.reports[0].kind != "corrupt" {
		t.Fatalf("reports = %+v, want one corrupt report", rec.reports)
	}
	if got, want := rec.reports[0].path, filepath.Base(root); got != want {
		t.Errorf("report path = %q, want %q (the local name; %q renders as a bare dot)", got, want, "")
	}
	if !strings.Contains(rec.reports[0].err.Error(), root) {
		t.Errorf("the recovery does not name the root's full path: %v", rec.reports[0].err)
	}
}
