package transfer

import (
	"context"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/otherworld/nimbo/internal/engine"
)

// applyMove performs a rename on one side and rebaselines the path. MoveLocal is
// a remote rename (rename the local file to match); MoveRemote is a local rename
// (perform a server-side MOVE). Neither re-transfers content.
func (e *Executor) applyMove(ctx context.Context, a engine.Action) error {
	switch a.Kind {
	case engine.ActMoveLocal:
		from, to := e.localPath(a.Path), e.localPath(a.Dest)
		if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			return err
		}
		if err := os.Rename(from, to); err != nil {
			return err
		}
	case engine.ActMoveRemote:
		from, to := e.remotePath(a.Path), e.remotePath(a.Dest)
		if parent := path.Dir(to); parent != "." && parent != "/" {
			if err := e.Client.EnsureCollection(ctx, parent); err != nil {
				return err
			}
		}
		if err := e.Client.Move(ctx, from, to); err != nil {
			return err
		}
	}
	slog.Info(a.Kind.String(), "from", a.Path, "to", a.Dest)
	if err := e.deleteBaseline(a.Path); err != nil {
		return err
	}
	return e.rebaselineFile(ctx, a.Dest)
}

// resolution describes how a conflict was settled, for accurate reporting.
type resolution int

const (
	resKeptBoth    resolution = iota // genuinely divergent: both versions kept
	resIdentical                     // false alarm: same bytes on both sides
	resResurrected                   // a delete vs. edit: the edited version was kept
)

// ConflictPolicy controls what the executor does when it meets a conflict.
type ConflictPolicy int

const (
	PolicyAuto   ConflictPolicy = iota // resolve automatically — keep both (CLI default)
	PolicyAsk                          // defer to the user; only identical is auto-merged
	PolicyNewest                       // auto-resolve edited-file conflicts to the newer side
)

// Choice is a user's decision for a deferred conflict. The semantics are "make
// the other side match this side".
type Choice int

const (
	ChoiceKeepLocal  Choice = iota // upload local (or delete remote if local is gone)
	ChoiceKeepRemote               // download remote (or delete local if remote is gone)
	ChoiceKeepBoth                 // preserve both surviving versions
)

// ConflictInfo describes a deferred conflict awaiting a user choice.
type ConflictInfo struct {
	Path         string
	Kind         string // edited | deleted-locally | deleted-remotely | type
	LocalExists  bool
	RemoteExists bool
	// Each side's size and modified time, captured here at detection so the UI can
	// show what's being chosen between without a live round trip when the list opens.
	LocalSize   int64
	LocalMTime  time.Time
	RemoteSize  int64
	RemoteMTime time.Time
}

// classifyConflict inspects a conflict under PolicyAsk. It auto-merges identical
// content (returns merged=true); otherwise it returns the ConflictInfo to defer.
func (e *Executor) classifyConflict(ctx context.Context, a engine.Action) (info ConflictInfo, merged bool, err error) {
	rel := a.Path
	localP := e.localPath(rel)
	remoteEntry, remoteExists := e.Remote[rel]
	localInfo, lerr := os.Stat(localP)
	localExists := lerr == nil

	info = ConflictInfo{Path: rel, LocalExists: localExists, RemoteExists: remoteExists}
	// Capture per-side metadata once, now, so the conflict list renders instantly
	// later. Local size/mtime are free (already stat'd); one remote Stat gets the
	// server's size + mtime (cheap next to the content download we do below).
	if localExists && lerr == nil && !localInfo.IsDir() {
		info.LocalSize = localInfo.Size()
		info.LocalMTime = localInfo.ModTime()
	}
	if remoteExists {
		info.RemoteSize = remoteEntry.Size
		if e.Client != nil { // best-effort: one Stat for the server mtime
			if ent, ok, serr := e.Client.Stat(ctx, e.remotePath(rel)); serr == nil && ok {
				info.RemoteSize = ent.Size
				info.RemoteMTime = ent.LastModified
			}
		}
	}
	switch {
	case localExists && !remoteExists:
		info.Kind = "deleted-remotely"
		return info, false, nil
	case !localExists && remoteExists:
		info.Kind = "deleted-locally"
		return info, false, nil
	case !localExists && !remoteExists:
		return info, true, e.deleteBaseline(rel) // nothing to ask
	}
	if localInfo.IsDir() || remoteEntry.IsDir {
		// Type change can't be content-merged or sensibly chosen — auto keep-both.
		slog.Warn("type conflict (file vs directory) — keeping both", "path", rel)
		return info, true, e.resolveTypeConflict(rel)
	}

	// Two files: auto-merge only if byte-identical.
	localSHA, err := sha1File(localP)
	if err != nil {
		return info, false, err
	}
	tmp := localP + ".ncremote.tmp"
	dres, derr := Download(ctx, e.Client, e.remotePath(rel), tmp)
	if derr != nil {
		return info, false, derr
	}
	_ = os.Remove(tmp)
	if dres.ContentSHA1 == localSHA {
		slog.Info("conflict suppressed (identical content)", "path", rel)
		return info, true, e.rebaselineFile(ctx, rel)
	}
	info.Kind = "edited"
	return info, false, nil
}

// ApplyChoice settles a deferred conflict according to the user's choice.
func (e *Executor) ApplyChoice(ctx context.Context, rel string, c Choice) error {
	localP := e.localPath(rel)
	remoteP := e.remotePath(rel)
	_, lerr := os.Stat(localP)
	localExists := lerr == nil
	_, remoteExists, _ := e.Client.Stat(ctx, remoteP)

	switch c {
	case ChoiceKeepLocal:
		if localExists {
			fr, err := Upload(ctx, e.Client, localP, remoteP)
			if err != nil {
				return err
			}
			return e.saveFileBaseline(rel, fr)
		}
		if remoteExists { // local was deleted → honour it
			if err := e.Client.Delete(ctx, remoteP); err != nil {
				return err
			}
		}
		return e.deleteBaseline(rel)

	case ChoiceKeepRemote:
		if remoteExists {
			fr, err := Download(ctx, e.Client, remoteP, localP)
			if err != nil {
				return err
			}
			if fr.FileID == "" {
				if ent, ok, _ := e.Client.Stat(ctx, remoteP); ok {
					fr.FileID = ent.FileID
				}
			}
			return e.saveFileBaseline(rel, fr)
		}
		if localExists { // remote was deleted → honour it
			if err := os.RemoveAll(localP); err != nil {
				return err
			}
		}
		return e.deleteBaseline(rel)

	case ChoiceKeepBoth:
		switch {
		case localExists && remoteExists:
			return e.keepBoth(ctx, rel)
		case localExists:
			fr, err := Upload(ctx, e.Client, localP, remoteP)
			if err != nil {
				return err
			}
			return e.saveFileBaseline(rel, fr)
		case remoteExists:
			fr, err := Download(ctx, e.Client, remoteP, localP)
			if err != nil {
				return err
			}
			return e.saveFileBaseline(rel, fr)
		default:
			return e.deleteBaseline(rel)
		}
	}
	return nil
}

// resolveConflict handles a path that changed on both sides. The cases:
//
//   - one side deleted, the other modified → deletion loses; resurrect the
//     surviving edited version (download it back, or re-upload it). This protects
//     against an accidental delete wiping out a real edit made elsewhere.
//   - both still present, same bytes → false alarm; just rebaseline.
//   - both still present, different bytes → keep both (remote takes the original
//     name; the local copy is preserved as a "conflicted copy" on both sides).
//   - a file-vs-directory type change → left for manual resolution (surfaced,
//     not silently mishandled).
func (e *Executor) resolveConflict(ctx context.Context, a engine.Action) (res resolution, err error) {
	rel := a.Path
	localP := e.localPath(rel)

	remoteEntry, remoteExists := e.Remote[rel]
	localInfo, lerr := os.Stat(localP)
	localExists := lerr == nil

	switch {
	case localExists && !remoteExists:
		// Remote deleted, local modified → keep the local edit by re-uploading.
		fr, uerr := Upload(ctx, e.Client, localP, e.remotePath(rel))
		if uerr != nil {
			return resResurrected, uerr
		}
		slog.Warn("conflict: remote deletion overridden by local change (re-uploaded)", "path", rel)
		return resResurrected, e.saveFileBaseline(rel, fr)

	case !localExists && remoteExists:
		// Local deleted, remote modified → keep the remote edit by re-downloading.
		fr, derr := Download(ctx, e.Client, e.remotePath(rel), localP)
		if derr != nil {
			return resResurrected, derr
		}
		if fr.FileID == "" {
			fr.FileID = remoteEntry.FileID
		}
		slog.Warn("conflict: local deletion overridden by remote change (re-downloaded)", "path", rel)
		return resResurrected, e.saveFileBaseline(rel, fr)

	case !localExists && !remoteExists:
		// Both gone; nothing to keep — just forget it.
		return resResurrected, e.deleteBaseline(rel)
	}

	// Both sides still exist. A file-vs-directory change can't be content-merged;
	// keep both by setting the local item aside and letting the remote version
	// re-sync into place.
	if localInfo.IsDir() || remoteEntry.IsDir {
		slog.Warn("type conflict (file vs directory) — keeping both", "path", rel)
		return resKeptBoth, e.resolveTypeConflict(rel)
	}

	localSHA, err := sha1File(localP)
	if err != nil {
		return resKeptBoth, err
	}

	// Fetch the remote version to a temp file (the ".tmp" suffix keeps it out of
	// local scans) and learn its content hash from the download.
	tmp := localP + ".ncremote.tmp"
	dres, err := Download(ctx, e.Client, e.remotePath(rel), tmp)
	if err != nil {
		return resKeptBoth, err
	}
	if dres.FileID == "" {
		dres.FileID = e.Remote[rel].FileID
	}

	if dres.ContentSHA1 == localSHA {
		// Same bytes on both sides — not a real conflict.
		_ = os.Remove(tmp)
		slog.Info("conflict suppressed (identical content)", "path", rel)
		return resIdentical, e.rebaselineFile(ctx, rel)
	}

	// Genuinely divergent: keep both.
	_ = os.Remove(tmp)
	return resKeptBoth, e.keepBoth(ctx, rel)
}

// resolveTypeConflict handles a file↔directory change by renaming the local item
// aside (as a conflicted copy) and clearing the baseline, so the next reconcile
// brings the remote version down fresh and uploads the aside copy as new.
func (e *Executor) resolveTypeConflict(rel string) error {
	localP := e.localPath(rel)
	confLocal := e.localPath(conflictName(rel))
	if err := os.MkdirAll(filepath.Dir(confLocal), 0o755); err != nil {
		return err
	}
	if err := os.Rename(localP, confLocal); err != nil {
		return err
	}
	return e.deleteBaseline(rel)
}

// keepBoth preserves both versions: the local file is set aside as a "conflicted
// copy" (kept on both sides) and the remote version takes the original name.
func (e *Executor) keepBoth(ctx context.Context, rel string) error {
	localP := e.localPath(rel)
	confRel := conflictName(rel)
	confLocal := e.localPath(confRel)

	if err := os.MkdirAll(filepath.Dir(confLocal), 0o755); err != nil {
		return err
	}
	if err := os.Rename(localP, confLocal); err != nil {
		return err
	}
	dres, err := Download(ctx, e.Client, e.remotePath(rel), localP)
	if err != nil {
		return err
	}
	if dres.FileID == "" {
		if ent, ok, _ := e.Client.Stat(ctx, e.remotePath(rel)); ok {
			dres.FileID = ent.FileID
		}
	}
	if err := e.saveFileBaseline(rel, dres); err != nil {
		return err
	}
	ures, err := Upload(ctx, e.Client, confLocal, e.remotePath(confRel))
	if err != nil {
		return err
	}
	slog.Warn("conflict: keeping both versions", "path", rel, "localCopy", confRel)
	return e.saveFileBaseline(confRel, ures)
}

// rebaselineFile records the current synced state of a file by inspecting both
// sides — used after moves and suppressed conflicts where no transfer occurred.
func (e *Executor) rebaselineFile(ctx context.Context, rel string) error {
	fi, err := os.Stat(e.localPath(rel))
	if err != nil {
		return err
	}
	var etag, fileID string
	if ent, ok, err := e.Client.Stat(ctx, e.remotePath(rel)); err == nil && ok {
		etag, fileID = ent.ETag, ent.FileID
	}
	sha, _ := sha1File(e.localPath(rel))
	return e.saveFileBaseline(rel, FileResult{
		ETag: etag, FileID: fileID,
		Size: fi.Size(), MTimeNanos: fi.ModTime().UnixNano(),
		ContentSHA1: sha,
	})
}

// conflictName inserts a " (conflicted copy <timestamp>)" marker before the file
// extension, mirroring the convention users recognise from other sync clients.
func conflictName(rel string) string {
	ext := path.Ext(rel)
	stem := strings.TrimSuffix(rel, ext)
	ts := time.Now().Format("2006-01-02 150405")
	return stem + " (conflicted copy " + ts + ")" + ext
}
