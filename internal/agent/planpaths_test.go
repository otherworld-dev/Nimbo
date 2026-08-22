package agent

import (
	"testing"

	"github.com/otherworld/nimbo/internal/engine"
)

// A local move reaches the watcher as two paths: the old name gone, the new name
// appeared. Diffed independently that reads as "delete on the server" plus
// "upload a new file" — which is what the flyout was showing, and what really
// happened: the content went back up the wire and the old copy went to the
// server's trash, losing its version history.
//
// Both halves are in the same batch, so the pair can be recognised and turned
// into one server-side MOVE.
func TestPlanPathsCoalescesAMoveWithinTheBatch(t *testing.T) {
	base := map[string]engine.BaselineState{
		"old/report.txt": {Path: "old/report.txt", RemoteETag: "e1", RemoteFileID: "101", LocalSize: 12, ContentSHA1: "aaaa"},
	}
	remote := map[string]engine.RemoteState{
		"old/report.txt": {Path: "old/report.txt", ETag: "e1", FileID: "101", Size: 12},
	}
	local := map[string]engine.LocalState{
		"new/report.txt": {Path: "new/report.txt", Size: 12},
	}
	hashLocal := func(rel string) (string, error) { return "aaaa", nil }

	actions := planPaths(base, remote, local, hashLocal)

	if len(actions) != 1 {
		t.Fatalf("want a single move, got %d actions: %+v", len(actions), actions)
	}
	a := actions[0]
	if a.Kind != engine.ActMoveRemote {
		t.Fatalf("want ActMoveRemote, got %v (%s)", a.Kind, a.Reason)
	}
	if a.Path != "old/report.txt" || a.Dest != "new/report.txt" {
		t.Fatalf("want old/report.txt -> new/report.txt, got %q -> %q", a.Path, a.Dest)
	}
}

// The safety case: a real deletion must stay a deletion. Nothing new in the
// batch matches its content, so there is no move to infer.
func TestPlanPathsKeepsARealDeleteADelete(t *testing.T) {
	base := map[string]engine.BaselineState{
		"gone.txt": {Path: "gone.txt", RemoteETag: "e1", RemoteFileID: "101", LocalSize: 12, ContentSHA1: "aaaa"},
	}
	remote := map[string]engine.RemoteState{
		"gone.txt": {Path: "gone.txt", ETag: "e1", FileID: "101", Size: 12},
	}
	local := map[string]engine.LocalState{}

	actions := planPaths(base, remote, local, func(string) (string, error) { return "", nil })

	if len(actions) != 1 || actions[0].Kind != engine.ActDeleteRemote {
		t.Fatalf("a real delete must stay ActDeleteRemote, got %+v", actions)
	}
}

// And a genuinely new file must stay an upload — matching only kicks in when its
// content matches a baseline entry that is simultaneously disappearing.
func TestPlanPathsKeepsARealUploadAnUpload(t *testing.T) {
	base := map[string]engine.BaselineState{}
	remote := map[string]engine.RemoteState{}
	local := map[string]engine.LocalState{
		"fresh.txt": {Path: "fresh.txt", Size: 5},
	}

	actions := planPaths(base, remote, local, func(string) (string, error) { return "zzzz", nil })

	if len(actions) != 1 || actions[0].Kind != engine.ActUpload {
		t.Fatalf("a new file must stay ActUpload, got %+v", actions)
	}
}

// A file whose baseline row has no recorded content hash cannot be matched — the
// signature index is keyed on it. It must fall back to delete+upload rather than
// mis-pair with something unrelated.
func TestPlanPathsWithoutABaselineHashFallsBackToDeleteAndUpload(t *testing.T) {
	base := map[string]engine.BaselineState{
		"old/report.txt": {Path: "old/report.txt", RemoteETag: "e1", RemoteFileID: "101", LocalSize: 12}, // no ContentSHA1
	}
	remote := map[string]engine.RemoteState{
		"old/report.txt": {Path: "old/report.txt", ETag: "e1", FileID: "101", Size: 12},
	}
	local := map[string]engine.LocalState{
		"new/report.txt": {Path: "new/report.txt", Size: 12},
	}

	actions := planPaths(base, remote, local, func(string) (string, error) { return "aaaa", nil })

	kinds := map[engine.ActionKind]bool{}
	for _, a := range actions {
		kinds[a.Kind] = true
	}
	if !kinds[engine.ActDeleteRemote] || !kinds[engine.ActUpload] {
		t.Fatalf("want delete+upload fallback, got %+v", actions)
	}
}
