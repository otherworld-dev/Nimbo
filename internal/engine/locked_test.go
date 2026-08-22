package engine

import (
	"testing"

	"github.com/otherworld/nimbo/internal/transport"
)

func TestFilterLocked(t *testing.T) {
	byOther := &transport.LockInfo{Owner: "bob"}
	byUs := &transport.LockInfo{Owner: "alice"}
	remote := map[string]RemoteState{
		"theirs.xlsx":  {Path: "theirs.xlsx", LockKnown: true, Lock: byOther},
		"mine.xlsx":    {Path: "mine.xlsx", LockKnown: true, Lock: byUs},
		"free.xlsx":    {Path: "free.xlsx", LockKnown: true},
		"unknown.xlsx": {Path: "unknown.xlsx", LockKnown: false, Lock: byOther},
	}
	actions := []Action{
		{Kind: ActUpload, Path: "theirs.xlsx"},
		{Kind: ActUpload, Path: "mine.xlsx"},
		{Kind: ActUpload, Path: "free.xlsx"},
		{Kind: ActUpload, Path: "unknown.xlsx"},
		{Kind: ActUpload, Path: "absent.xlsx"},
		{Kind: ActDownload, Path: "theirs.xlsx"},
		{Kind: ActDeleteRemote, Path: "theirs.xlsx"},
	}

	kept, held := FilterLocked(actions, remote, "alice")

	if len(held) != 1 || held[0] != "theirs.xlsx" {
		t.Fatalf("held = %v, want only theirs.xlsx", held)
	}
	// A download of the locked file must survive: it is how we learn what they saved.
	var sawDownload, sawDelete bool
	for _, a := range kept {
		if a.Kind == ActUpload && a.Path == "theirs.xlsx" {
			t.Error("upload to a file someone else has locked was not held")
		}
		if a.Kind == ActDownload && a.Path == "theirs.xlsx" {
			sawDownload = true
		}
		if a.Kind == ActDeleteRemote {
			sawDelete = true
		}
	}
	if !sawDownload || !sawDelete {
		t.Errorf("non-upload actions were dropped: download=%v delete=%v", sawDownload, sawDelete)
	}
	if len(kept) != 6 {
		t.Errorf("kept %d actions, want 6", len(kept))
	}
}

// Unknown lock state must never hold an upload: a pruned subtree reports nil,
// and treating that as a lock would silently stop a user's work syncing.
func TestFilterLockedIgnoresUnknown(t *testing.T) {
	remote := map[string]RemoteState{
		"a.xlsx": {Path: "a.xlsx", LockKnown: false, Lock: &transport.LockInfo{Owner: "bob"}},
	}
	kept, held := FilterLocked([]Action{{Kind: ActUpload, Path: "a.xlsx"}}, remote, "alice")
	if len(held) != 0 || len(kept) != 1 {
		t.Errorf("kept=%v held=%v; an unexamined path must not be held", kept, held)
	}
}
