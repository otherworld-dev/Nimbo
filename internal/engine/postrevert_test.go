package engine

import "testing"

// The first pass after leaving virtual-files mode must never delete on the
// server. A folder whose placeholders never populated is empty on disk, and the
// reconciler reads that as a user deletion — which cost a real file on
// 2026-08-16.
func TestRestoreInsteadOfDelete(t *testing.T) {
	remote := map[string]RemoteState{
		"Team/Budget.xlsx": {Path: "Team/Budget.xlsx"},
		"Team":             {Path: "Team", IsDir: true},
	}
	actions := []Action{
		{Kind: ActDeleteRemote, Path: "Team/Budget.xlsx"},
		{Kind: ActDeleteRemote, Path: "Team"},
		{Kind: ActDeleteRemote, Path: "Gone/other.txt"}, // not on the server
		{Kind: ActUpload, Path: "mine.txt"},
		{Kind: ActDeleteLocal, Path: "removed-remotely.txt"},
	}

	out, restored := RestoreInsteadOfDelete(actions, remote)

	byPath := map[string]ActionKind{}
	for _, a := range out {
		byPath[a.Path] = a.Kind
	}
	if byPath["Team/Budget.xlsx"] != ActDownload {
		t.Errorf("file: got %v, want a download", byPath["Team/Budget.xlsx"])
	}
	if byPath["Team"] != ActCreateLocalDir {
		t.Errorf("directory: got %v, want a local mkdir", byPath["Team"])
	}
	// Deleting LOCALLY because the server no longer has it is unrelated and safe.
	if byPath["removed-remotely.txt"] != ActDeleteLocal {
		t.Errorf("local delete was altered: %v", byPath["removed-remotely.txt"])
	}
	if byPath["mine.txt"] != ActUpload {
		t.Errorf("upload was altered: %v", byPath["mine.txt"])
	}
	// Nothing on the server to restore from: leave the action as it was rather
	// than inventing a download of something that does not exist.
	if byPath["Gone/other.txt"] != ActDeleteRemote {
		t.Errorf("unknown path: got %v", byPath["Gone/other.txt"])
	}
	if len(restored) != 2 {
		t.Errorf("restored = %v, want the two server-backed paths", restored)
	}
	if len(out) != len(actions) {
		t.Errorf("action count changed: %d -> %d", len(actions), len(out))
	}
}
