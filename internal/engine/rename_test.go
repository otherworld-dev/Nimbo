package engine

import "testing"

// TestCoalesce_RemoteRename: a file moved on the server (same oc:fileid at the
// new path) should become a single MoveLocal, not delete+download.
func TestCoalesce_RemoteRename(t *testing.T) {
	base := map[string]BaselineState{
		"old.txt": {Path: "old.txt", RemoteFileID: "F1", RemoteETag: "e1", LocalSize: 10, LocalMTimeNanos: t0.UnixNano(), ContentSHA1: "h"},
	}
	remote := map[string]RemoteState{
		"new.txt": remoteWithID("new.txt", "e2", 10, "F1"),
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
