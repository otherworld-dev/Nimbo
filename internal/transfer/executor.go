package transfer

import (
	"context"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
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
				slog.Error("conflict classification failed", "path", a.Path, "err", err)
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

// applyTransfer performs a single download or upload and records the baseline.
func (e *Executor) applyTransfer(ctx context.Context, a engine.Action) error {
	remote := e.remotePath(a.Path)
	local := e.localPath(a.Path)

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
		if err == nil || ctx.Err() != nil {
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
	return e.saveFileBaseline(a.Path, res)
}

// applyDelete removes a path on the side that no longer should have it and drops
// its baseline row.
func (e *Executor) applyDelete(ctx context.Context, a engine.Action) error {
	if a.Kind == engine.ActDeleteLocal {
		clearReadOnlyTree(e.localPath(a.Path)) // mirrored read-only files block RemoveAll on Windows
		if err := os.RemoveAll(e.localPath(a.Path)); err != nil {
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

// clearReadOnlyTree strips the read-only attribute from a path and everything
// under it, so a RemoveAll of a mirrored read-only subtree succeeds on Windows.
func clearReadOnlyTree(root string) {
	if fi, err := os.Stat(root); err != nil || (!fi.IsDir() && fi.Mode()&0o200 != 0) {
		if err == nil {
			_ = os.Chmod(root, 0o644)
		}
		return
	}
	_ = filepath.Walk(root, func(p string, _ os.FileInfo, err error) error {
		if err == nil {
			_ = os.Chmod(p, 0o644)
		}
		return nil
	})
}

func (e *Executor) makeLocalDir(rel string) error {
	if err := os.MkdirAll(e.localPath(rel), 0o755); err != nil {
		return err
	}
	r := e.Remote[rel]
	_ = setReadOnly(e.localPath(rel), r.ReadOnly) // mirror a read-only server folder
	return e.saveDirBaseline(rel, r.ETag, r.FileID)
}

func (e *Executor) makeRemoteDir(ctx context.Context, rel string) error {
	remote := e.remotePath(rel)
	if err := e.Client.Mkcol(ctx, remote); err != nil {
		return err
	}
	// Fetch the created collection's metadata for an accurate baseline.
	var etag, fileID string
	if ent, ok, err := e.Client.Stat(ctx, remote); err == nil && ok {
		etag, fileID = ent.ETag, ent.FileID
	}
	return e.saveDirBaseline(rel, etag, fileID)
}

// remotePath maps a pair-relative path to a files-root-relative path.
func (e *Executor) remotePath(rel string) string {
	rel = e.Escaper.Encode(rel) // encode a forbidden name to its stored server name; no-op when inactive
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
	})
}

func (e *Executor) saveDirBaseline(rel, etag, fileID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.State.UpsertBaseline(e.PairKey, engine.BaselineState{
		Path: rel, IsDir: true, RemoteETag: etag, RemoteFileID: fileID,
	})
}

func (e *Executor) deleteBaseline(rel string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.State.DeleteBaseline(e.PairKey, rel)
}

func sortByPathAsc(a []engine.Action)  { sort.Slice(a, func(i, j int) bool { return a[i].Path < a[j].Path }) }
func sortByPathDesc(a []engine.Action) { sort.Slice(a, func(i, j int) bool { return a[i].Path > a[j].Path }) }

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
