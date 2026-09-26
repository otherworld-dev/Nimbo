package transfer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/otherworld/nimbo/internal/engine"
	"github.com/otherworld/nimbo/internal/state"
	"github.com/otherworld/nimbo/internal/transport"
)

// Executor applies an engine plan to the network and filesystem and records the
// results in the baseline. It maps the plan's pair-relative paths onto concrete
// remote (files-root-relative) and local (filesystem) paths.
type Executor struct {
	Client     *transport.Client
	State      *state.Store
	PairKey    string                        // baseline identity for this local↔remote pair
	LocalRoot  string                        // local filesystem root of the pair
	RemoteRoot string                        // files-root-relative remote root
	Remote     map[string]engine.RemoteState // current remote scan, for dir metadata
	Workers    int                           // max concurrent transfers (default 4)
	// Escaper, when active, maps a server-forbidden local name to its stored server
	// name (e.g. .htaccess -> .htaccess.nimboesc) for every server op. Nil/inactive
	// is a no-op — remotePath returns the path unchanged.
	Escaper *engine.Escaper
	// OnEvent, if set, is called after each operation (success or failure) so a
	// UI can record activity and errors. It must be safe for concurrent use.
	OnEvent func(a engine.Action, err error)
	// OnBegin, if set, is called just before a transfer starts (used to surface
	// in-progress state, e.g. shell overlay icons). Safe for concurrent use.
	OnBegin func(a engine.Action)
	// OnProgress, if set, is called with the bytes transferred for an action as
	// they flow, for live progress display. Safe for concurrent use.
	OnProgress func(a engine.Action, delta int64)
	// OnMovedAside, if set, is told when a folder or file the server deleted was
	// too big for the Recycle Bin and was moved next to the sync folder instead.
	OnMovedAside func(rel, dest string)
	binUsed      map[string]int64 // bytes this run put in each drive's Recycle Bin

	// Policy controls conflict handling. Under PolicyAsk, conflicts (other than
	// identical content) are deferred into Pending instead of auto-resolved.
	Policy  ConflictPolicy
	Pending []ConflictInfo

	mu sync.Mutex // guards State writes and stats from transfer goroutines
}

// newestChoice picks the side with the more recent modification time for an
// edited-file conflict (PolicyNewest). Ties and lookup failures favour remote.
func (e *Executor) newestChoice(ctx context.Context, a engine.Action) Choice {
	var lt, rt time.Time
	if fi, err := os.Stat(e.localPath(a.Path)); err == nil {
		lt = fi.ModTime()
	}
	if ent, ok, err := e.Client.Stat(ctx, e.remotePath(a.Path)); err == nil && ok {
		rt = ent.LastModified
	}
	if lt.After(rt) {
		return ChoiceKeepLocal
	}
	return ChoiceKeepRemote
}

// report notifies the OnEvent hook, if one is set.
func (e *Executor) report(a engine.Action, err error) {
	if e.OnEvent != nil {
		e.OnEvent(a, err)
	}
}

// Stats summarises what an executor run did.
type Stats struct {
	Downloaded, Uploaded int
	MkLocal, MkRemote    int
	DelLocal, DelRemote  int
	Moved                int
	Conflicts            int // conflicts resolved via keep-both
	ConflictsIdentical   int // conflicts that were false alarms (identical content)
	ConflictsResurrected int // delete-vs-edit conflicts where the edit was kept
	Failed               int
}

// Plus returns the field-wise sum of two Stats, for aggregating several scoped
// syncs into one result.
func (s Stats) Plus(o Stats) Stats {
	return Stats{
		Downloaded: s.Downloaded + o.Downloaded, Uploaded: s.Uploaded + o.Uploaded,
		MkLocal: s.MkLocal + o.MkLocal, MkRemote: s.MkRemote + o.MkRemote,
		DelLocal: s.DelLocal + o.DelLocal, DelRemote: s.DelRemote + o.DelRemote,
		Moved:                s.Moved + o.Moved,
		Conflicts:            s.Conflicts + o.Conflicts,
		ConflictsIdentical:   s.ConflictsIdentical + o.ConflictsIdentical,
		ConflictsResurrected: s.ConflictsResurrected + o.ConflictsResurrected,
		Failed:               s.Failed + o.Failed,
	}
}

// Run executes the actions. Ordering preserves correctness: create directories
// (parents first), then transfer files concurrently, then process deletions
// (children first). Conflicts are reported but not resolved (Phase 4).
func (e *Executor) Run(ctx context.Context, actions []engine.Action) (Stats, error) {
	if e.Workers <= 0 {
		e.Workers = 4
	}
	var stats Stats

	var mkLocal, mkRemote, moves, transfers, deletes, conflicts []engine.Action
	for _, a := range actions {
		switch a.Kind {
		case engine.ActCreateLocalDir:
			mkLocal = append(mkLocal, a)
		case engine.ActCreateRemoteDir:
			mkRemote = append(mkRemote, a)
		case engine.ActMoveLocal, engine.ActMoveRemote:
			moves = append(moves, a)
		case engine.ActDownload, engine.ActUpload:
			transfers = append(transfers, a)
		case engine.ActDeleteLocal, engine.ActDeleteRemote:
			deletes = append(deletes, a)
		case engine.ActConflict:
			conflicts = append(conflicts, a)
		}
	}

	// 1. Directories, parents before children.
	sortByPathAsc(mkLocal)
	for _, a := range mkLocal {
		err := e.makeLocalDir(a.Path)
		e.report(a, err)
		if err != nil {
			// Raw detail at Debug only; the engine logs a deduped, human-readable
			// version once per item (see Engine.recordActionResult) to avoid per-pass spam.
			slog.Debug("mkdir-local failed", "path", a.Path, "err", err)
			stats.Failed++
			continue
		}
		stats.MkLocal++
	}
	sortByPathAsc(mkRemote)
	for _, a := range mkRemote {
		err := e.makeRemoteDir(ctx, a.Path)
		e.report(a, err)
		if err != nil {
			slog.Debug("mkdir-remote failed", "path", a.Path, "err", err) // see recordActionResult for the user-facing log
			stats.Failed++
			continue
		}
		stats.MkRemote++
	}

	// 2. Moves/renames (cheap — no re-transfer).
	for _, a := range moves {
		err := e.applyMove(ctx, a)
		e.report(a, err)
		if err != nil {
			slog.Error("move failed", "kind", a.Kind, "from", a.Path, "to", a.Dest, "err", err)
			stats.Failed++
			continue
		}
		stats.Moved++
	}

	// 3. File transfers, concurrently.
	e.runTransfers(ctx, transfers, &stats)

	// 4. Conflicts: suppress false alarms, then handle per policy.
	for _, a := range conflicts {
		if e.Policy == PolicyAsk || e.Policy == PolicyNewest {
			info, merged, err := e.classifyConflict(ctx, a)
			if err != nil {
				e.report(a, err)
				var inUse *InUseError
				if errors.As(err, &inUse) {
					slog.Info("conflict check waits: the file is in use", "path", a.Path)
				} else {
					slog.Error("conflict classification failed", "path", a.Path, "err", err)
				}
				stats.Failed++
				continue
			}
			if merged {
				stats.ConflictsIdentical++
				continue
			}
			if e.Policy == PolicyAsk {
				e.Pending = append(e.Pending, info) // defer to the user
				continue
			}
			// PolicyNewest: auto-resolve an edited file to its newer side; for
			// delete/type conflicts fall back to the safe keep-both path.
			if info.Kind == "edited" {
				c := e.newestChoice(ctx, a)
				if err := e.ApplyChoice(ctx, a.Path, c); err != nil {
					e.report(a, err)
					slog.Error("keep-newest resolution failed", "path", a.Path, "err", err)
					stats.Failed++
					continue
				}
				e.report(a, nil)
				stats.Conflicts++
				continue
			}
		}
		outcome, err := e.resolveConflict(ctx, a)
		e.report(a, err)
		if err != nil {
			slog.Error("conflict resolution failed", "path", a.Path, "err", err)
			stats.Failed++
			continue
		}
		switch outcome {
		case resIdentical:
			stats.ConflictsIdentical++
		case resResurrected:
			stats.ConflictsResurrected++
		default:
			stats.Conflicts++
		}
	}

	// 5. Deletions, children before parents.
	sortByPathDesc(deletes)
	for _, a := range deletes {
		err := e.applyDelete(ctx, a)
		e.report(a, err)
		if err != nil {
			slog.Error("delete failed", "kind", a.Kind, "path", a.Path, "err", err)
			stats.Failed++
			continue
		}
		if a.Kind == engine.ActDeleteLocal {
			stats.DelLocal++
		} else {
			stats.DelRemote++
		}
	}

	return stats, nil
}

// runTransfers downloads/uploads with a bounded worker pool.
func (e *Executor) runTransfers(ctx context.Context, transfers []engine.Action, stats *Stats) {
	sem := make(chan struct{}, e.Workers)
	var wg sync.WaitGroup
	for _, a := range transfers {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(a engine.Action) {
			defer wg.Done()
			defer func() { <-sem }()
			if e.OnBegin != nil {
				e.OnBegin(a)
			}
			err := e.applyTransfer(ctx, a)
			e.report(a, err)
			if errors.Is(err, ErrUploadInProgress) {
				// Another pass is sending this file right now and reports the
				// outcome itself; if it fails, the file is still changed and the
				// next pass sends it. Neither a failure nor an upload here.
				slog.Debug("upload left to the one already running", "path", a.Path)
				return
			}
			if err != nil {
				slog.Debug("transfer failed", "kind", a.Kind, "path", a.Path, "err", err) // see recordActionResult for the user-facing log
				e.mu.Lock()
				stats.Failed++
				e.mu.Unlock()
				return
			}
			e.mu.Lock()
			if a.Kind == engine.ActDownload {
				stats.Downloaded++
			} else {
				stats.Uploaded++
			}
			e.mu.Unlock()
		}(a)
	}
	wg.Wait()
}

// redundantDownload reports whether a planned download would fetch bytes the
// local file already has, and returns the local content hash so the caller can
// rebaseline without hashing twice.
//
// A server-side metadata change bumps the ETag without touching content — taking
// or releasing a files_lock lock does exactly that, as do tags, comments and
// favourites — and the diff sees only the ETag. Without this guard every peer
// re-downloads the whole file twice per lock cycle.
//
// Deliberately conservative: no server checksum, a size mismatch, or any error
// means "download it".
func (e *Executor) redundantDownload(rel string) (string, bool) {
	r, ok := e.Remote[rel]
	if !ok || r.IsDir || r.SHA1 == "" {
		return "", false
	}
	fi, err := os.Stat(e.localPath(rel))
	if err != nil || fi.IsDir() || fi.Size() != r.Size {
		return "", false
	}
	localSHA, err := sha1File(e.localPath(rel))
	if err != nil || localSHA == "" {
		return "", false
	}
	// A save landing during the hash would be absorbed into the baseline as
	// "unchanged" and never uploaded — refuse the shortcut if the file moved.
	if cur, serr := os.Stat(e.localPath(rel)); serr != nil ||
		cur.Size() != fi.Size() || !cur.ModTime().Equal(fi.ModTime()) {
		return "", false
	}
	return localSHA, strings.EqualFold(localSHA, r.SHA1)
}

// rebaselineUnchanged records the server's new ETag against unchanged local
// content, so a metadata-only change stops looking like a pending download. It
// makes no network call: the ETag and file id come from the listing that
// produced this plan, and the hash was computed by redundantDownload.
func (e *Executor) rebaselineUnchanged(rel, localSHA string) error {
	fi, err := os.Stat(e.localPath(rel))
	if err != nil {
		return err
	}
	r := e.Remote[rel]
	return e.saveFileBaseline(rel, FileResult{
		ETag: r.ETag, FileID: r.FileID,
		Size: fi.Size(), MTimeNanos: fi.ModTime().UnixNano(),
		ContentSHA1: localSHA,
	})
}

// applyTransfer performs a single download or upload and records the baseline.
func (e *Executor) applyTransfer(ctx context.Context, a engine.Action) error {
	remote := e.remotePath(a.Path)
	local := e.localPath(a.Path)

	// A download whose bytes we already hold is pure waste — see redundantDownload.
	if a.Kind == engine.ActDownload {
		if localSHA, redundant := e.redundantDownload(a.Path); redundant {
			slog.Info("download skipped (metadata-only change, content identical)", "path", a.Path)
			return e.rebaselineUnchanged(a.Path, localSHA)
		}
	}

	// Retry transient failures with backoff. Resume (range download / chunk
	// upload) makes retries cheap; context cancellation stops immediately.
	const maxAttempts = 3
	var res FileResult
	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			if berr := sleepBackoff(ctx, attempt); berr != nil {
				return berr
			}
			slog.Debug("retrying transfer", "kind", a.Kind, "path", a.Path, "attempt", attempt+1)
		}
		var prog func(int64)
		if e.OnProgress != nil {
			prog = func(n int64) { e.OnProgress(a, n) }
		}
		if a.Kind == engine.ActDownload {
			res, err = DownloadProgress(ctx, e.Client, remote, local, prog)
		} else {
			res, err = UploadProgress(ctx, e.Client, local, remote, prog)
		}
		// A lock is not a transient failure — somebody else has the file and will
		// have it for as long as they have it. Retrying just delays the message.
		// The same goes for every other deliberate refusal (quota, forbidden,
		// auth): re-hashing a huge file two more times won't change the answer.
		// And for a file another program is writing: Outlook keeps an attached
		// .pst open for hours, so it waits for the next pass. A checksum mismatch
		// is the server's copy being damaged: fetching it again brings back the
		// same bytes, and on a 24 GB file each try costs minutes.
		var inUse *InUseError
		var damaged *ChecksumMismatchError
		// Another upload of this file running is the same: it is not ours to retry.
		if err == nil || ctx.Err() != nil || transport.IsLocked(err) || !transport.Retryable(err) ||
			errors.As(err, &inUse) || errors.As(err, &damaged) || errors.Is(err, ErrUploadInProgress) {
			break
		}
	}
	if err != nil {
		return err
	}
	// Not all servers send OC-FileId/OC-ETag on GET; the PROPFIND scan always has
	// them, and an accurate fileid is what makes future rename detection work.
	if a.Kind == engine.ActDownload {
		if r, ok := e.Remote[a.Path]; ok {
			if res.FileID == "" {
				res.FileID = r.FileID
			}
			if res.ETag == "" {
				res.ETag = r.ETag
			}
		}
	}
	slog.Info(a.Kind.String(), "path", a.Path, "size", res.Size)
	if a.Kind == engine.ActDownload {
		_ = setReadOnly(local, e.Remote[a.Path].ReadOnly) // mirror server read-only
	}
	if err := e.saveFileBaseline(a.Path, res); err != nil {
		return err
	}
	return nil
}

// applyDelete removes a path on the side that no longer should have it and drops
// its baseline row.
func (e *Executor) applyDelete(ctx context.Context, a engine.Action) error {
	if a.Kind == engine.ActDeleteLocal {
		// Recycle Bin first: a deletion mirrored FROM the server is the one
		// this PC never chose, so it keeps an undo. Below the damage guard's
		// thresholds this is the only safety net a server-side deletion has.
		if err := e.removeMirrored(a.Path); err != nil {
			return err
		}
	} else {
		if err := e.Client.Delete(ctx, e.remotePath(a.Path)); err != nil {
			return err
		}
	}
	slog.Info(a.Kind.String(), "path", a.Path)
	return e.deleteBaseline(a.Path)
}

// removeMirrored removes rel for a deletion that came from the server. The
// Recycle Bin keeps it where the bin can: an item bigger than the bin's
// capacity is deleted outright by Windows, silently with confirmation off,
// which is how 219 GB of "To Sort" was lost for good (Deck #691). Anything the
// bin can't keep, or refuses, is moved next to the sync folder instead, and
// OnMovedAside says where. Where even that fails, the item stays and the
// action fails: never a delete nobody can undo. Where there is no bin at all
// (another platform, a network or removable drive, a bin switched off) it
// stays a plain delete, as it always was.
func (e *Executor) removeMirrored(rel string) error {
	p := e.localPath(rel)
	capacity, hasBins := binCapacity(p)
	clearReadOnlyTree(p)
	if !hasBins || capacity == 0 {
		// No bin on this drive (network, removable) or the user switched it
		// off: deleting here was always for good, and still is.
		return os.RemoveAll(p)
	}
	if capacity == binUnknown {
		return e.putAside(rel) // a bin whose size we couldn't read: don't gamble
	}
	// The bin takes items only until this pass has put nine tenths of its
	// capacity in. It makes room by purging its oldest items, so a folder
	// arriving file by file (a full or delta pass lists everything in it) would
	// otherwise push its own first files out for good.
	vol := strings.ToLower(filepath.VolumeName(p))
	e.mu.Lock()
	room := capacity/10*9 - e.binUsed[vol]
	e.mu.Unlock()
	if size, fits := sizeWithin(p, room); fits {
		if err := recycleFn(p); err == nil {
			e.mu.Lock()
			if e.binUsed == nil {
				e.binUsed = make(map[string]int64)
			}
			e.binUsed[vol] += size
			e.mu.Unlock()
			return nil
		}
	}
	return e.putAside(rel)
}

// putAside moves rel next to the sync folder for a deletion the Recycle Bin
// can't keep, and says so.
func (e *Executor) putAside(rel string) error {
	dest, err := e.moveAside(rel)
	if err != nil {
		return fmt.Errorf("too big for the Recycle Bin, and could not move it aside: %w", err)
	}
	slog.Warn("deleted on the server and too big for the Recycle Bin: moved aside", "path", rel, "to", dest)
	if e.OnMovedAside != nil {
		e.OnMovedAside(rel, dest)
	}
	return nil
}

// sizeWithin adds up everything at p and reports whether it comes to no more
// than limit bytes, stopping as soon as it doesn't.
func sizeWithin(p string, limit int64) (int64, bool) {
	var total int64
	if limit < 0 {
		return 0, false
	}
	over := errors.New("over")
	err := filepath.WalkDir(p, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if total += fi.Size(); total > limit {
			return over
		}
		return nil
	})
	return total, err == nil
}

// moveAside moves rel out of the sync folder into a sibling named after it,
// "<sync folder> - removed on server", keeping its relative path so it is
// recognisable, and never over something already there.
func (e *Executor) moveAside(rel string) (string, error) {
	root := filepath.Clean(e.LocalRoot)
	base := filepath.Base(root)
	if base == "." || base == string(filepath.Separator) || base == filepath.VolumeName(root) {
		return "", fmt.Errorf("the sync folder %s is a drive root, with no folder beside it", root)
	}
	dest := filepath.Join(filepath.Dir(root), base+" - removed on server", filepath.FromSlash(rel))
	if _, err := os.Lstat(dest); err == nil {
		for i := 2; ; i++ {
			c := fmt.Sprintf("%s (%d)", dest, i)
			if _, err := os.Lstat(c); err != nil {
				dest = c
				break
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(e.localPath(rel), dest); err != nil {
		return "", err
	}
	return dest, nil
}

// RemoveToBin removes a local path the way a mirrored server deletion does:
// to the Recycle Bin where there is one, by a plain delete where there is not
// (non-Windows, or a path the shell refuses). Mirrored read-only attributes
// are cleared first, since they block deletion on Windows.
func RemoveToBin(path string) error {
	clearReadOnlyTree(path)
	if err := recycle(path); err == nil {
		return nil
	}
	return os.RemoveAll(path)
}

// clearReadOnlyTree strips the read-only attribute from a path and everything
// under it, so a RemoveAll of a mirrored read-only subtree succeeds on Windows.
// It only adds owner bits: setting a flat 0644 took the search bit off every
// folder on Unix, so the delete it prepares failed with permission denied and
// left folders nobody could open. Folders get the owner's rwx back, which also
// frees one an older build left like that.
func clearReadOnlyTree(root string) {
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		if err == nil {
			makeWritable(root, fi)
		}
		return
	}
	_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err == nil {
			makeWritable(p, fi)
		}
		return nil
	})
}

func makeWritable(p string, fi os.FileInfo) {
	want := fi.Mode().Perm() | 0o200
	if fi.IsDir() {
		want |= 0o700
	}
	if want != fi.Mode().Perm() {
		_ = os.Chmod(p, want)
	}
}

func (e *Executor) makeLocalDir(rel string) error {
	if err := os.MkdirAll(e.localPath(rel), 0o755); err != nil {
		return err
	}
	r := e.Remote[rel]
	_ = setReadOnly(e.localPath(rel), r.ReadOnly) // mirror a read-only server folder
	return e.saveDirBaseline(rel, r.ETag, r.FileID, r.MountRoot)
}

func (e *Executor) makeRemoteDir(ctx context.Context, rel string) error {
	remote := e.rawRemotePath(rel) // a folder is never escaped
	if err := e.Client.Mkcol(ctx, remote); err != nil {
		return err
	}
	// Fetch the created collection's metadata for an accurate baseline.
	var etag, fileID string
	if ent, ok, err := e.Client.Stat(ctx, remote); err == nil && ok {
		etag, fileID = ent.ETag, ent.FileID
	}
	return e.saveDirBaseline(rel, etag, fileID, false) // our own new folder: never a share root
}

// remotePath maps a pair-relative FILE path to a files-root-relative path,
// encoding a forbidden name to its stored server name (no-op when inactive).
func (e *Executor) remotePath(rel string) string {
	return e.rawRemotePath(e.Escaper.Encode(rel))
}

// rawRemotePath is remotePath without the encoding: for a directory, which is
// never escaped (escaping renames a basename only, and a folder's children keep
// their own path underneath it).
func (e *Executor) rawRemotePath(rel string) string {
	if e.RemoteRoot == "" {
		return rel
	}
	return path.Join(e.RemoteRoot, rel)
}

// localPath maps a pair-relative path to a filesystem path.
func (e *Executor) localPath(rel string) string {
	return filepath.Join(e.LocalRoot, filepath.FromSlash(rel))
}

func (e *Executor) saveFileBaseline(rel string, res FileResult) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.State.UpsertBaseline(e.PairKey, engine.BaselineState{
		Path: rel, IsDir: false,
		RemoteETag: res.ETag, RemoteFileID: res.FileID,
		LocalSize: res.Size, LocalMTimeNanos: res.MTimeNanos,
		ContentSHA1: res.ContentSHA1,
		ContentKey:  e.listedContentKey(rel, res.ETag),
		// A single file shared with the user is a mount root of its own; the
		// listing said so. A path the listing never saw (a fresh upload) reads
		// as the zero value, i.e. not one.
		MountRoot: e.Remote[rel].MountRoot,
	})
}

// listedContentKey is the content key of the version the listing showed for
// rel, but only when etag says that is the version just synced. An upload
// makes a version the listing never saw, and the old key must not vouch for
// it: "" then, and the ETag alone decides until the next listing.
func (e *Executor) listedContentKey(rel, etag string) string {
	r, ok := e.Remote[rel]
	if !ok || etag == "" || r.ETag != etag {
		return ""
	}
	return r.ContentKey()
}

func (e *Executor) saveDirBaseline(rel, etag, fileID string, mountRoot bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.State.UpsertBaseline(e.PairKey, engine.BaselineState{
		Path: rel, IsDir: true, RemoteETag: etag, RemoteFileID: fileID, MountRoot: mountRoot,
	})
}

// deleteBaseline drops rel's row and, when rel was a folder, the rows of
// everything in it: those files went with it, and rows left behind would read
// next time as deletions to send to the other side (Deck #691).
func (e *Executor) deleteBaseline(rel string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.State.DeleteBaselineUnder(e.PairKey, rel)
}

func sortByPathAsc(a []engine.Action) {
	sort.Slice(a, func(i, j int) bool { return a[i].Path < a[j].Path })
}
func sortByPathDesc(a []engine.Action) {
	sort.Slice(a, func(i, j int) bool { return a[i].Path > a[j].Path })
}

// sleepBackoff waits an exponential delay for the given (1-based) retry attempt,
// or returns early if the context is cancelled.
func sleepBackoff(ctx context.Context, attempt int) error {
	d := time.Duration(1<<uint(attempt-1)) * 500 * time.Millisecond
	if d > 5*time.Second {
		d = 5 * time.Second
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

var (
	binCapacity = volumeBinCapacity
	recycleFn   = recycle
)

// binUnknown is the capacity reported for a drive that has a Recycle Bin whose
// size couldn't be read.
const binUnknown int64 = -1
