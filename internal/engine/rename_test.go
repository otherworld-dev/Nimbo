package engine

import "testing"

// TestCoalesce_RemoteRename: a file moved on the server (same oc:fileid at the
// new path) should become a single MoveLocal, not delete+download.
func TestCoalesce_RemoteRename(t *testing.T) {
	base := map[string]BaselineState{
		"old.txt": {Path: "old.txt", RemoteFileID: "F1", RemoteETag: "e1", LocalSize: 10, LocalMTimeNanos: t0.UnixNano(), ContentSHA1: "h"},
	}
	remote := map[string]RemoteState{
		"new.txt": remoteWithID("new.txt", "e1", 10, "F1"), // same version: the etag did not change
	}
	local := map[string]LocalState{
		"old.txt": local("old.txt", 10, t0), // unchanged locally — only moved on server
	}

	actions := CoalesceRenames(Diff(base, remote, local), base, remote, local, nil)

	mv := find(actions, "old.txt")
	if mv.Kind != ActMoveLocal || mv.Dest != "new.txt" {
		t.Fatalf("expected MoveLocal old.txt->new.txt, got %+v", mv)
	}
	for _, a := range actions {
		if a.Kind == ActDownload || a.Kind == ActDeleteLocal {
			t.Errorf("rename not coalesced: leftover %s on %s", a.Kind, a.Path)
		}
	}
}

// TestCoalesce_LocalRename: a file renamed locally (same content+size as a
// baseline entry now gone locally) should become a single MoveRemote.
func TestCoalesce_LocalRename(t *testing.T) {
	base := map[string]BaselineState{
		"old.txt": {Path: "old.txt", RemoteFileID: "F1", RemoteETag: "e1", LocalSize: 10, ContentSHA1: "h"},
	}
	remote := map[string]RemoteState{
		"old.txt": remote("old.txt", "e1", 10), // unchanged on server
	}
	local := map[string]LocalState{
		"new.txt": local("new.txt", 10, t0),
	}
	hashLocal := func(rel string) (string, error) {
		if rel == "new.txt" {
			return "h", nil // same content as baseline old.txt
		}
		return "", nil
	}

	actions := CoalesceRenames(Diff(base, remote, local), base, remote, local, hashLocal)

	mv := find(actions, "old.txt")
	if mv.Kind != ActMoveRemote || mv.Dest != "new.txt" {
		t.Fatalf("expected MoveRemote old.txt->new.txt, got %+v", mv)
	}
	for _, a := range actions {
		if a.Kind == ActUpload || a.Kind == ActDeleteRemote {
			t.Errorf("rename not coalesced: leftover %s on %s", a.Kind, a.Path)
		}
	}
}

// TestCoalesce_NoFalsePositive: a genuinely new download with an unrelated
// fileid must not be turned into a move.
func TestCoalesce_NoFalsePositive(t *testing.T) {
	base := map[string]BaselineState{
		"old.txt": {Path: "old.txt", RemoteFileID: "F1", LocalSize: 10, ContentSHA1: "h"},
	}
	remote := map[string]RemoteState{
		"new.txt": remoteWithID("new.txt", "e2", 10, "DIFFERENT"),
		"old.txt": remote("old.txt", "e1", 10),
	}
	local := map[string]LocalState{
		"old.txt": local("old.txt", 10, t0),
	}
	actions := CoalesceRenames(Diff(base, remote, local), base, remote, local, nil)
	if a := find(actions, "new.txt"); a.Kind != ActDownload {
		t.Errorf("expected plain Download for unrelated file, got %s", a.Kind)
	}
}

func remoteWithID(path, etag string, size int64, id string) RemoteState {
	return RemoteState{Path: path, ETag: etag, Size: size, FileID: id}
}

// renamePlan is a one-file remote rename old.txt -> new.txt with the pieces a
// test wants to vary.
func renamePlan(mut func(base map[string]BaselineState, remote map[string]RemoteState, local map[string]LocalState)) []Action {
	base := map[string]BaselineState{
		"old.txt": {Path: "old.txt", RemoteFileID: "F1", RemoteETag: "e1", LocalSize: 10, LocalMTimeNanos: t0.UnixNano(), ContentSHA1: "h"},
	}
	remote := map[string]RemoteState{"new.txt": remoteWithID("new.txt", "e1", 10, "F1")}
	local := map[string]LocalState{"old.txt": local("old.txt", 10, t0)}
	if mut != nil {
		mut(base, remote, local)
	}
	return CoalesceRenames(Diff(base, remote, local), base, remote, local, nil)
}

func hasKind(actions []Action, k ActionKind) bool {
	for _, a := range actions {
		if a.Kind == k {
			return true
		}
	}
	return false
}

// A rename is only a rename if the content is the same version. A file that
// kept its oc:fileid but was rewritten (ransomware renaming as it encrypts, a
// restore) used to be coalesced on the id alone: the local file was renamed and
// its OLD content recorded as in sync with the server's NEW version, and the
// damage guard, which does not count moves, never saw the replacement
// (Deck #947). It stays delete + download, which the guard counts.
func TestCoalesce_RemoteRenameNeedsTheSameContent(t *testing.T) {
	changed := renamePlan(func(_ map[string]BaselineState, remote map[string]RemoteState, _ map[string]LocalState) {
		remote["new.txt"] = remoteWithID("new.txt", "e2", 10, "F1") // new etag, nothing to vouch for the content
	})
	if hasKind(changed, ActMoveLocal) || !hasKind(changed, ActDownload) || !hasKind(changed, ActDeleteLocal) {
		t.Errorf("a moved AND changed file was coalesced into a rename: %+v", changed)
	}

	// Nextcloud bumps the ETag for locks, tags and the like while the bytes stay
	// the same; the content key (size, mtime, upload time) vouches for those.
	key := remoteWithID("new.txt", "e2", 10, "F1")
	key.LastModified, key.UploadTime = t0, 1700000000
	sameKey := renamePlan(func(base map[string]BaselineState, remote map[string]RemoteState, _ map[string]LocalState) {
		b := base["old.txt"]
		b.ContentKey = key.ContentKey()
		base["old.txt"] = b
		remote["new.txt"] = key
	})
	if !hasKind(sameKey, ActMoveLocal) {
		t.Errorf("same content key, different etag: want a rename, got %+v", sameKey)
	}
	otherKey := key
	otherKey.UploadTime++ // re-uploaded
	diffKey := renamePlan(func(base map[string]BaselineState, remote map[string]RemoteState, _ map[string]LocalState) {
		b := base["old.txt"]
		b.ContentKey = key.ContentKey()
		base["old.txt"] = b
		remote["new.txt"] = otherKey
	})
	if hasKind(diffKey, ActMoveLocal) {
		t.Errorf("different content key: must not be a rename, got %+v", diffKey)
	}
}

// A rename never lands on a path that already has a file. The local rename
// replaces its target, so a file moved over a tracked one destroyed that
// file's local copy without a download ever being planned, and so without the
// damage guard counting a replacement (Deck #947).
func TestCoalesce_RemoteRenameNeverOntoAnExistingPath(t *testing.T) {
	// new.txt was already synced; the server now has old.txt's file there.
	tracked := renamePlan(func(base map[string]BaselineState, _ map[string]RemoteState, local map[string]LocalState) {
		base["new.txt"] = BaselineState{Path: "new.txt", RemoteFileID: "F2", RemoteETag: "e9", LocalSize: 20, LocalMTimeNanos: t0.UnixNano()}
		local["new.txt"] = localFile("new.txt", 20)
	})
	if hasKind(tracked, ActMoveLocal) {
		t.Errorf("a rename was planned onto a tracked file: %+v", tracked)
	}
	// An untracked local file sits where the renamed file would go.
	untracked := renamePlan(func(_ map[string]BaselineState, _ map[string]RemoteState, local map[string]LocalState) {
		local["new.txt"] = localFile("new.txt", 20)
	})
	if hasKind(untracked, ActMoveLocal) {
		t.Errorf("a rename was planned onto a local file that is in the way: %+v", untracked)
	}
}

func localFile(path string, size int64) LocalState {
	return LocalState{Path: path, Size: size, MTime: t0}
}
