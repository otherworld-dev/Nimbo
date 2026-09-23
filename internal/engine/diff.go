package engine

import (
	"sort"
	"strings"
)

// Diff performs a three-way reconciliation of the baseline (last synced),
// remote, and local states, returning the actions needed to converge them. It
// is a pure function of its inputs — no I/O — which makes the reconciliation
// rules exhaustively testable.
//
// The rules, per path, hinge on which side(s) changed relative to the baseline:
//   - remote changed  := remote present and (no baseline or etag differs)
//   - local changed   := local present and (no baseline or size/mtime differ)
//
// Conflicts (both sides changed) are reported, not resolved; Phase 3 executes
// the unambiguous actions and Phase 4 adds content-hash conflict suppression
// and rename detection via FileID.
func Diff(base map[string]BaselineState, remote map[string]RemoteState, local map[string]LocalState) []Action {
	paths := unionKeys(base, remote, local)
	sort.Strings(paths)

	actions := make([]Action, 0, len(paths))
	for _, p := range paths {
		b, hasB := base[p]
		r, hasR := remote[p]
		l, hasL := local[p]
		if a := classify(p, b, hasB, r, hasR, l, hasL); a.Kind != ActNoop {
			actions = append(actions, a)
		}
	}
	return actions
}

// classify decides the action for a single path given its presence and state on
// each of the three sides.
func classify(p string, b BaselineState, hasB bool, r RemoteState, hasR bool, l LocalState, hasL bool) Action {
	switch {
	case hasR && hasL:
		return classifyBoth(p, b, hasB, r, l)
	case hasR && !hasL:
		return classifyRemoteOnly(p, b, hasB, r)
	case !hasR && hasL:
		return classifyLocalOnly(p, b, hasB, l)
	default:
		// Present only in a stale baseline (deleted on both sides). Nothing to
		// transfer; the baseline row will be pruned during execution.
		return noop(p)
	}
}

// sameContentAsBaseline reports whether the server's copy has the SAME bytes as
// the last synced state despite a different ETag.
//
// Nextcloud bumps a file's ETag for metadata-only changes — taking or releasing
// a files_lock lock does it, as do tags, comments and favourites. Without this
// check the diff reads that as "remote changed", which is wrong twice over: on
// its own it forces a pointless re-download of every peer's copy, and combined
// with a genuine local edit it manufactures a conflict and a conflicted copy out
// of a file nobody touched on the server.
//
// Two proofs are accepted, each only when known on BOTH sides: matching SHA1
// checksums, or a matching content key (size, mtime and nc:upload_time — see
// transport.ContentKey), which is what covers the many files the server holds
// no checksum for (GitHub #7). Anything unknown falls back to trusting the ETag.
func sameContentAsBaseline(r RemoteState, b BaselineState) bool {
	if r.IsDir {
		return false
	}
	if r.SHA1 != "" && b.ContentSHA1 != "" {
		return strings.EqualFold(r.SHA1, b.ContentSHA1)
	}
	k := r.ContentKey()
	return k != "" && k == b.ContentKey
}

// classifyBoth handles a path present on both remote and local.
func classifyBoth(p string, b BaselineState, hasB bool, r RemoteState, l LocalState) Action {
	if r.IsDir && l.IsDir {
		return noop(p) // directory exists both sides; children handled separately
	}
	if r.IsDir != l.IsDir {
		return act(ActConflict, p, "type mismatch: directory on one side, file on the other")
	}

	remoteChanged := !hasB || (r.ETag != b.RemoteETag && !sameContentAsBaseline(r, b))
	localChanged := !hasB || localFileChanged(l, b)

	switch {
	case !remoteChanged && !localChanged:
		return noop(p)
	case remoteChanged && !localChanged:
		return act(ActDownload, p, "remote changed")
	case localChanged && !remoteChanged:
		return act(ActUpload, p, "local changed")
	case !hasB:
		// New on both sides with no common ancestor. Same size is a strong hint
		// they're identical; the executor confirms via content hash before
		// deciding whether it's a real conflict.
		if r.Size == l.Size {
			return act(ActConflict, p, "appeared on both sides, same size (verify content)")
		}
		return act(ActConflict, p, "appeared on both sides with different content")
	default:
		return act(ActConflict, p, "modified on both sides since last sync")
	}
}

// classifyRemoteOnly handles a path present remotely but not locally.
func classifyRemoteOnly(p string, b BaselineState, hasB bool, r RemoteState) Action {
	if !hasB {
		if r.IsDir {
			return act(ActCreateLocalDir, p, "new on remote")
		}
		return act(ActDownload, p, "new on remote")
	}
	// It was synced before and is now gone locally → deleted locally.
	if !r.IsDir && r.ETag != b.RemoteETag {
		return act(ActConflict, p, "deleted locally but modified remotely")
	}
	return act(ActDeleteRemote, p, "removed locally")
}

// classifyLocalOnly handles a path present locally but not remotely.
func classifyLocalOnly(p string, b BaselineState, hasB bool, l LocalState) Action {
	if !hasB {
		if l.IsDir {
			return act(ActCreateRemoteDir, p, "new locally")
		}
		return act(ActUpload, p, "new locally")
	}
	// It was synced before and is now gone remotely → deleted remotely.
	if !l.IsDir && localFileChanged(l, b) {
		return act(ActConflict, p, "deleted remotely but modified locally")
	}
	return act(ActDeleteLocal, p, "removed remotely")
}

// DeadBaselinePaths returns the baseline paths present on NEITHER side — stale
// rows for entries deleted (or newly ignored/excluded) everywhere. classify()
// maps these to noop with the promise that the row is pruned during execution,
// and the caller performs that prune. Leaving them is not harmless: any row
// whose nearest listed ancestor is re-scanned surfaces as a "changed path" on
// every pass, keeping the remote-delta off its no-change fast path (observed in
// the field: 1,652 rows for long-deleted top-level folders re-diffed every few
// minutes, forever). Pruning a row for a path that still exists under an
// ignore/exclude just means a later un-ignore re-adopts it via the
// new-on-both-sides content-verify path — no data is touched either way.
func DeadBaselinePaths(base map[string]BaselineState, remote map[string]RemoteState, local map[string]LocalState) []string {
	var dead []string
	for p := range base {
		if _, ok := remote[p]; ok {
			continue
		}
		if _, ok := local[p]; ok {
			continue
		}
		dead = append(dead, p)
	}
	return dead
}

// localFileChanged reports whether a local file differs from its baseline by the
// cheap signals available without hashing: size and modification time.
func localFileChanged(l LocalState, b BaselineState) bool {
	return l.Size != b.LocalSize || l.MTime.UnixNano() != b.LocalMTimeNanos
}

// act builds a single-path action with a reason.
func act(kind ActionKind, path, reason string) Action {
	return Action{Kind: kind, Path: path, Reason: reason}
}

func noop(p string) Action { return Action{Kind: ActNoop, Path: p} }

// unionKeys returns the deduplicated set of paths across all three maps.
func unionKeys(base map[string]BaselineState, remote map[string]RemoteState, local map[string]LocalState) []string {
	seen := make(map[string]struct{}, len(base)+len(remote)+len(local))
	for k := range base {
		seen[k] = struct{}{}
	}
	for k := range remote {
		seen[k] = struct{}{}
	}
	for k := range local {
		seen[k] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	return out
}
