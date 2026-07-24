package engine

import "strconv"

// CoalesceRenames rewrites delete+create action pairs that are really the same
// file having moved into single, cheap move actions — avoiding a full
// re-transfer when a file is only renamed or relocated.
//
//   - Remote rename (the server moved X→Y, e.g. via the web UI): the new remote
//     Y carries the same oc:fileid as a baseline entry X that is now gone
//     remotely but still present locally — which the diff expresses as
//     DeleteLocal(X) + Download(Y). Rewritten to MoveLocal(X→Y): just rename the
//     local file. This is reliable because oc:fileid is stable across moves.
//
//   - Local rename (the user moved X→Y): the new local Y has the same content
//     hash and size as a baseline entry X that is now gone locally — expressed
//     as DeleteRemote(X) + Upload(Y). Rewritten to MoveRemote(X→Y): a server-side
//     MOVE, no upload. Requires hashLocal to hash Y's content; pass nil to skip
//     local-rename detection.
func CoalesceRenames(
	actions []Action,
	base map[string]BaselineState,
	remote map[string]RemoteState,
	local map[string]LocalState,
	hashLocal func(rel string) (string, error),
) []Action {
	byFileID := make(map[string]string) // oc:fileid -> baseline path
	bySig := make(map[string]string)    // "sha1|size" -> baseline path
	for p, b := range base {
		if b.IsDir {
			continue
		}
		if b.RemoteFileID != "" {
			byFileID[b.RemoteFileID] = p
		}
		if b.ContentSHA1 != "" {
			bySig[sig(b.ContentSHA1, b.LocalSize)] = p
		}
	}

	delLocalIdx := make(map[string]int)
	delRemoteIdx := make(map[string]int)
	for i, a := range actions {
		switch a.Kind {
		case ActDeleteLocal:
			delLocalIdx[a.Path] = i
		case ActDeleteRemote:
			delRemoteIdx[a.Path] = i
		}
	}

	consumed := make([]bool, len(actions))
	out := make([]Action, 0, len(actions))

	for i, a := range actions {
		if consumed[i] {
			continue
		}
		switch a.Kind {
		case ActDownload:
			if x, ok := matchRemoteRename(a.Path, remote, byFileID, delLocalIdx, consumed); ok {
				consumed[delLocalIdx[x]] = true
				consumed[i] = true
				out = append(out, Action{Kind: ActMoveLocal, Path: x, Dest: a.Path, Reason: "renamed on server"})
				continue
			}
		case ActUpload:
			if x, ok := matchLocalRename(a.Path, base, local, bySig, delRemoteIdx, consumed, hashLocal); ok {
				consumed[delRemoteIdx[x]] = true
				consumed[i] = true
				out = append(out, Action{Kind: ActMoveRemote, Path: x, Dest: a.Path, Reason: "renamed locally"})
				continue
			}
		}
		out = append(out, a)
	}
	return out
}

// matchRemoteRename finds the baseline path X that the new remote Y was renamed
// from, via a shared oc:fileid and a pending DeleteLocal(X).
func matchRemoteRename(y string, remote map[string]RemoteState, byFileID map[string]string, delLocalIdx map[string]int, consumed []bool) (string, bool) {
	r, ok := remote[y]
	if !ok || r.FileID == "" {
		return "", false
	}
	x, ok := byFileID[r.FileID]
	if !ok {
		return "", false
	}
	if di, ok := delLocalIdx[x]; !ok || consumed[di] {
		return "", false
	}
	return x, true
}

// matchLocalRename finds the baseline path X that the new local Y was renamed
// from, via matching content signature and a pending DeleteRemote(X).
func matchLocalRename(y string, base map[string]BaselineState, local map[string]LocalState, bySig map[string]string, delRemoteIdx map[string]int, consumed []bool, hashLocal func(string) (string, error)) (string, bool) {
	if hashLocal == nil {
		return "", false
	}
	if _, hasB := base[y]; hasB {
		return "", false // not a new file
	}
	ly, ok := local[y]
	if !ok || ly.IsDir {
		return "", false
	}
	h, err := hashLocal(y)
	if err != nil || h == "" {
		return "", false
	}
	x, ok := bySig[sig(h, ly.Size)]
	if !ok {
		return "", false
	}
	if di, ok := delRemoteIdx[x]; !ok || consumed[di] {
		return "", false
	}
	return x, true
}

// sig builds a content signature key from a hash and size.
func sig(sha1hex string, size int64) string {
	return sha1hex + "|" + strconv.FormatInt(size, 10)
}
