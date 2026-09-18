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

	out, restored := RestoreInsteadOfDelete(actions, remote, nil)

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

// Deck #678. An online-only stub is deleted by the revert; when its server copy
// had ALSO changed since the last live baseline, the diff sees "in the
// baseline, on the server with a new etag, missing locally" and calls it a
// conflict ("deleted locally but modified remotely"). Nobody deleted anything:
// the stub simply never held the bytes, and the forecast dialog promised those
// files would download after the switch. In the post-revert window that
// conflict is a download too.
func TestRestoreInsteadOfDeleteRedownloadsAMissingConflict(t *testing.T) {
	remote := map[string]RemoteState{
		"test file.txt":  {Path: "test file.txt", ETag: "new"},
		"edited.txt":     {Path: "edited.txt", ETag: "new"},
		"Team":           {Path: "Team", IsDir: true},
		"Team/inner.txt": {Path: "Team/inner.txt", ETag: "n2"},
	}
	missing := map[string]bool{"test file.txt": true, "Team/inner.txt": true}
	actions := []Action{
		{Kind: ActConflict, Path: "test file.txt", Reason: "deleted locally but modified remotely"},
		{Kind: ActConflict, Path: "Team/inner.txt", Reason: "deleted locally but modified remotely"},
		// Present locally: a real both-sides edit, still the user's to resolve.
		{Kind: ActConflict, Path: "edited.txt", Reason: "modified on both sides since last sync"},
		// A type mismatch has both sides present; never touched.
		{Kind: ActConflict, Path: "Team", Reason: "type mismatch: directory on one side, file on the other"},
		// Missing locally but gone from the server too: nothing to download.
		{Kind: ActConflict, Path: "vanished.txt", Reason: "deleted locally but modified remotely"},
	}

	out, restored := RestoreInsteadOfDelete(actions, remote, func(p string) bool { return missing[p] })

	byPath := map[string]ActionKind{}
	for _, a := range out {
		byPath[a.Path] = a.Kind
	}
	if byPath["test file.txt"] != ActDownload || byPath["Team/inner.txt"] != ActDownload {
		t.Errorf("missing-locally conflicts not turned into downloads: %v / %v", byPath["test file.txt"], byPath["Team/inner.txt"])
	}
	if byPath["edited.txt"] != ActConflict {
		t.Errorf("a both-sides edit must stay a conflict: %v", byPath["edited.txt"])
	}
	if byPath["Team"] != ActConflict {
		t.Errorf("a type mismatch must stay a conflict: %v", byPath["Team"])
	}
	if byPath["vanished.txt"] != ActConflict {
		t.Errorf("a conflict with nothing on the server must stay as it was: %v", byPath["vanished.txt"])
	}
	if len(restored) != 2 {
		t.Errorf("restored = %v, want the two re-downloads", restored)
	}
	if len(out) != len(actions) {
		t.Errorf("action count changed: %d -> %d", len(actions), len(out))
	}

	// Without a way to tell what is on disk, conflicts are left alone — never
	// guess a download over a file the user may have edited.
	out, restored = RestoreInsteadOfDelete(actions, remote, nil)
	for _, a := range out {
		if a.Kind != ActConflict {
			t.Errorf("nil predicate rewrote %q to %v", a.Path, a.Kind)
		}
	}
	if len(restored) != 0 {
		t.Errorf("nil predicate restored %v", restored)
	}
}
