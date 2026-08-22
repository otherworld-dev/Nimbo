//go:build windows

package vfs

// Adopting an existing folder into virtual-files mode.
//
// Mounting a cloud sync root over a folder that already holds files leaves those
// files outside the placeholder system: cfapi.Mount only seeds the root when the
// directory is empty, and reconcileDir only refreshes entries that cfInspect
// reports as clean placeholders — a plain file fails that test and can go stale
// unnoticed. Adopt closes that gap for both the live -> on-demand switch and a
// migration from another client's virtual-files folder.
//
// The work is split into three stages with hard ordering constraints:
//
//	Scan          — pure, read-only. Before the mount; cancel is free.
//	Apply         — every LOCAL mutation (marks, stub deletes, conflict
//	                renames). MUST run after the sync root is mounted but
//	                BEFORE its write-back watcher starts: the watcher pushes
//	                local changes to the server as user actions, so a stub
//	                delete seen by the watcher becomes a SERVER-side delete,
//	                and a conflict rename becomes a server-side MOVE racing
//	                our own upload.
//	UploadPending — the slow network half (uploads + their post-upload
//	                marks). Mutates nothing locally except the final mark, so
//	                it is safe to run in the background under a live watcher.
//
// Design: docs/specs/2026-07-26-vfs-adopt-existing-folder-design.md.

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/otherworld/nimbo/internal/cfapi"
	"github.com/otherworld/nimbo/internal/engine"
	"github.com/otherworld/nimbo/internal/transfer"
)

// Action is what adopt will do with one pre-existing local entry.
type Action int

const (
	// ActionKeep: the local file already holds the server's content. It is
	// marked in-sync so it stays hydrated and is never re-fetched.
	ActionKeep Action = iota
	// ActionConflict: present on both sides but different. Both versions are
	// kept: the local copy is renamed to a "conflicted copy" and uploaded under
	// that name; the original path is placeholdered from the server.
	ActionConflict
	// ActionUpload: not on the server. Uploaded first, and marked in-sync only
	// once the server has it (see the invariant on UploadPending).
	ActionUpload
	// ActionReplace: a dehydrated placeholder holding no content — typically
	// another client's, orphaned once that client is uninstalled. Deleted, which
	// leaves the path remote-only for reconcile to recreate as ours.
	ActionReplace
)

func (a Action) String() string {
	switch a {
	case ActionKeep:
		return "keep"
	case ActionConflict:
		return "conflict"
	case ActionUpload:
		return "upload"
	case ActionReplace:
		return "replace"
	}
	return "unknown"
}

// Entry is one classified local file. Size and MTimeNanos are the SCAN-TIME
// observations: Apply re-stats each file and skips any that changed since — the
// confirm dialog can sit open indefinitely while syncing continues, so the plan
// may be stale by the time it runs.
type Entry struct {
	Rel        string `json:"rel"` // sync-root-relative, forward slashes
	Action     Action `json:"action"`
	Size       int64  `json:"size"`
	MTimeNanos int64  `json:"mtimeNanos"`
	// RemoteRel is the SERVER-side name for this entry — differs from Rel when
	// name-escaping is active (local .htaccess ↔ server .htaccess.nimboesc).
	// Identities and upload targets must use it: marking the local name gave
	// reconcile an identity the server doesn't have, and uploading it put
	// literal forbidden names on the server (live incident).
	RemoteRel string `json:"remoteRel"`
}

// Plan is a scan's result: every pre-existing local file, classified.
// Remote-only paths are deliberately absent — reconcile already creates those
// placeholders, and duplicating it would be a second thing to keep correct.
type Plan struct {
	Entries []Entry `json:"entries"`
	// remoteName maps local rel -> server name (see Scan); kept so Apply can
	// name conflicted copies correctly on the server too.
	remoteName func(rel string) string
}

// UploadItem is one pending upload: the local rel path and the server-side
// name it must be stored under (escaped when escaping is active).
type UploadItem struct {
	Rel       string
	RemoteRel string
}

// Counts totals the entries per action, for the confirmation summary.
func (p Plan) Counts() map[Action]int {
	out := make(map[Action]int, 4)
	for _, e := range p.Entries {
		out[e.Action]++
	}
	return out
}

// UploadBytes is how much would be sent to the server, so the user is told the
// cost before committing (a wrongly-picked folder could be enormous).
func (p Plan) UploadBytes() int64 {
	var n int64
	for _, e := range p.Entries {
		if e.Action == ActionUpload || e.Action == ActionConflict {
			n += e.Size
		}
	}
	return n
}

// Scan classifies every file under localDir against remote (keyed by
// sync-root-relative path, as engine.RemoteScan returns).
//
// It is pure: no cfapi calls, nothing mutated. That is what lets it run BEFORE
// the folder is mounted, so the user can be shown a summary and cancel for free.
// Directories are not classified — they are implied by their contents, and an
// empty one is recreated by reconcile from the server listing. (Known gap: an
// empty LOCAL-only directory is never adopted; it stays a plain dir.)
//
// skip (optional) is the sync ignore predicate (same rules live mode uses,
// root-relative forward-slash paths): matches are excluded from the plan
// entirely and not descended into — they stay untouched plain local files.
// Without it, a dev tree's node_modules/.git land in the "upload" bucket and
// the dialog offers to push gigabytes of junk to the server (live incident).
//
// remoteName (optional) maps a local rel path to its server-side name (the
// escaper's Encode); nil means names are identical. remote must be keyed by
// LOCAL (decoded) names.
func Scan(localDir string, remote map[string]engine.RemoteState, skip func(rel string) bool, remoteName func(rel string) string) (Plan, error) {
	var plan Plan
	err := filepath.WalkDir(localDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == localDir {
				// An unreadable ROOT must fail the scan, not read as an empty
				// folder — an empty plan looks like "nothing to adopt".
				return err
			}
			// An unreadable subtree shouldn't sink the whole scan; skip it and
			// let those files be picked up by a later pass.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if path == localDir {
			return nil
		}
		rel, rerr := filepath.Rel(localDir, path)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if skipName(rel) || (skip != nil && skip(rel)) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		fi, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		rr := rel
		if remoteName != nil {
			rr = remoteName(rel)
		}
		plan.Entries = append(plan.Entries, Entry{
			Rel:        rel,
			Action:     classify(fi, remote[rel], hasRemote(remote, rel)),
			Size:       fi.Size(),
			MTimeNanos: fi.ModTime().UnixNano(),
			RemoteRel:  rr,
		})
		return nil
	})
	if err != nil {
		return Plan{}, fmt.Errorf("scan %s: %w", localDir, err)
	}
	plan.remoteName = remoteName
	return plan, nil
}

func hasRemote(remote map[string]engine.RemoteState, rel string) bool {
	_, ok := remote[rel]
	return ok
}

// classify decides one entry's action. Order matters: the dehydrated test comes
// first because such a file reports the server's size and mtime while holding no
// bytes, so every other test below would wrongly read it as already present.
func classify(fi os.FileInfo, r engine.RemoteState, onServer bool) Action {
	if cfapi.IsDehydrated(fi) {
		return ActionReplace
	}
	if !onServer {
		return ActionUpload
	}
	if engine.LocalMatchesRemote(fi, r) {
		return ActionKeep
	}
	return ActionConflict
}

// ApplyResult reports what phase A did and what remains for phase B.
type ApplyResult struct {
	// Uploads are what phase B must upload then mark: local-only files plus
	// the renamed conflicted copies, each with its server-side target name.
	Uploads  []UploadItem
	Kept     int // matching files marked in-sync
	Replaced int // dead stubs deleted (reconcile recreates them as ours)
	Renamed  int // conflicts renamed to conflicted copies
	Skipped  int // entries whose file changed since the scan — left alone
	Failed   int // entries whose action errored — left alone
}

// AdoptOps are the side effects UploadPending needs, injected so the logic
// stays testable. A nil hook makes its action a no-op.
type AdoptOps struct {
	// Upload sends the local file at rel to the server under remoteRel (they
	// differ when name-escaping is active). Returning nil means the server now
	// has it — which is what licenses marking it in-sync.
	Upload func(rel, remoteRel string) error
	// Log records progress; optional.
	Log func(format string, args ...any)
}

func (o AdoptOps) logf(format string, args ...any) {
	if o.Log != nil {
		o.Log(format, args...)
	}
}

// Apply performs every local mutation of the plan. Call it only after the sync
// root is mounted (MarkInSync needs a registered root) and STRICTLY BEFORE the
// mount's write-back watcher starts — see the package comment for why.
//
// It can convert hundreds of thousands of files (minutes of work), so progress
// (optional) is called once per entry with (done, total), and ctx cancellation
// stops the run at the next entry boundary: converted files stay converted
// (they are correct), the rest stay plain files a re-scan re-offers.
//
// THE INVARIANT: a file is marked in-sync only when it is confirmed present on
// the server. MarkInSync turns a file into a clean placeholder, and reconcileDir
// deletes a clean placeholder that has no remote counterpart, reading it as a
// server-side deletion. Marking an unconfirmed file therefore hands it to the
// deleter. Hence only Keep entries are marked here; uploads mark in phase B
// after the server confirms, and a skipped/failed entry is never marked.
//
// Every entry re-verifies against the disk before acting (the plan may be
// stale), entries are independent (one failure cannot strand the rest), and
// every step is idempotent — a crash mid-run is recovered by re-scanning.
func (p Plan) Apply(ctx context.Context, localDir, remoteRoot string, progress func(done, total int)) ApplyResult {
	var res ApplyResult
	total := len(p.Entries)
	for i, e := range p.Entries {
		if ctx != nil && ctx.Err() != nil {
			return res // cancelled — every completed entry stands, the rest wait for a re-scan
		}
		if progress != nil {
			progress(i+1, total)
		}
		full := filepath.Join(localDir, filepath.FromSlash(e.Rel))
		switch e.Action {
		case ActionKeep:
			fi, err := os.Stat(full)
			if err != nil || !unchangedSince(fi, e) || cfapi.IsDehydrated(fi) {
				res.Skipped++
				continue
			}
			if err := adoptMark(full, remoteRoot, e.RemoteRel); err != nil {
				res.Failed++
				continue
			}
			res.Kept++
		case ActionConflict:
			fi, err := os.Stat(full)
			if err != nil || !unchangedSince(fi, e) || cfapi.IsDehydrated(fi) {
				res.Skipped++
				continue
			}
			conf := transfer.ConflictName(e.Rel)
			if err := os.Rename(full, filepath.Join(localDir, filepath.FromSlash(conf))); err != nil {
				res.Failed++
				continue
			}
			res.Renamed++
			confRemote := conf
			if p.remoteName != nil {
				confRemote = p.remoteName(conf)
			}
			res.Uploads = append(res.Uploads, UploadItem{Rel: conf, RemoteRel: confRemote})
		case ActionUpload:
			// No local mutation; the upload itself happens in phase B.
			res.Uploads = append(res.Uploads, UploadItem{Rel: e.Rel, RemoteRel: e.RemoteRel})
		case ActionReplace:
			// Re-verify it is STILL dehydrated: if it hydrated (or was replaced
			// by a real file) since the scan, it now holds content and deleting
			// it would destroy data.
			fi, err := os.Stat(full)
			if err != nil || !cfapi.IsDehydrated(fi) {
				res.Skipped++
				continue
			}
			// Deleting a dehydrated placeholder does not read it, so nothing is
			// downloaded and no content is lost — it never had any.
			if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
				// Leave it: without the delete, creating a placeholder at this
				// name would collide.
				res.Failed++
				continue
			}
			res.Replaced++
		}
	}
	return res
}

// unchangedSince reports whether the file on disk still matches the scan-time
// observation exactly (size + mtime nanos).
func unchangedSince(fi os.FileInfo, e Entry) bool {
	return fi.Size() == e.Size && fi.ModTime().UnixNano() == e.MTimeNanos
}

// UploadPending is phase B: upload each pending file, marking it in-sync only
// after its upload succeeds (the invariant on Apply). It mutates nothing else
// locally, so it is safe under a live watcher: a pre-existing plain file
// generates no filesystem events by being read, and the post-upload mark leaves
// the file in exactly the state the watcher treats as "clean, nothing to do".
// Returns the number of files that failed (they stay plain local files; a
// re-run of the adopt picks them up).
func UploadPending(localDir, remoteRoot string, uploads []UploadItem, ops AdoptOps) (failed int) {
	if ops.Upload == nil {
		return len(uploads)
	}
	for _, u := range uploads {
		if err := ops.Upload(u.Rel, u.RemoteRel); err != nil {
			// NOT marked: the server does not have it, so marking would expose
			// it to the reconcile deleter.
			ops.logf("adopt: upload %s: %v", u.Rel, err)
			failed++
			continue
		}
		full := filepath.Join(localDir, filepath.FromSlash(u.Rel))
		if err := adoptMark(full, remoteRoot, u.RemoteRel); err != nil {
			ops.logf("adopt: mark uploaded %s: %v", u.Rel, err)
			failed++
		}
	}
	return failed
}

// adoptMark converts a real file into a clean placeholder carrying its remote
// path as identity, so the watcher neither re-uploads nor re-fetches it. For a
// file that is ALREADY a placeholder (a hydrated one left by another client),
// MarkInSync would keep the foreign identity blob — dehydrating and re-opening
// it later would hand our hydrate callback an identity we can't serve. So the
// identity is repointed first; on a plain file that call fails harmlessly and
// MarkInSync does the conversion with the right identity itself.
func adoptMark(full, remoteRoot, rel string) error {
	identity := []byte(strings.Trim(remoteRoot+"/"+rel, "/"))
	_ = cfUpdateIdentity(full, identity)
	return cfMarkInSync(full, identity)
}
