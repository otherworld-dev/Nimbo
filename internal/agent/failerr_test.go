package agent

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/otherworld/nimbo/internal/engine"
	"github.com/otherworld/nimbo/internal/transport"
)

// TestHumanActionErr checks the raw → human-readable mapping, especially the
// .Collectives case (the one that prompted this).
func TestHumanActionErr(t *testing.T) {
	// The errors as the transport and the OS really produce them: HTTP failures
	// are StatusErrors, local ones *os.PathError (see TestActivityWordingGoesByType
	// for why the wording no longer reads the message).
	status := func(op, path string, code int, st string) error {
		return &transport.StatusError{Op: op, Path: path, Code: code, Status: st}
	}
	cases := []struct {
		path       string
		err        error
		wantSubstr string
	}{
		{".Collectives/Home Network", status("MKCOL", ".Collectives/Home Network", 507, "507 Insufficient Storage"), "Collectives"},
		{"docs/x.txt", status("PUT", "docs/x.txt", 403, "403 Forbidden"), "permission denied"},
		{"a/b", &os.PathError{Op: "mkdir", Path: `E:\Nextcloud\a`, Err: accessDenied}, "Windows denied access"},
		{"deep/file.md", status("MKCOL", "deep", 409, "409 Conflict"), "parent folder"},
		{"Team/Budget.xlsx", status("PUT", "Team/Budget.xlsx", 423, "423 Locked"), "someone else"},
		{"q/r", errors.New("some unrecognised transport error"), "some unrecognised transport error"}, // falls back to raw
	}
	for _, c := range cases {
		got := humanActionErr(engine.Action{Path: c.path}, c.err)
		if !strings.Contains(got, c.wantSubstr) {
			t.Errorf("humanActionErr(%q, %v) = %q, want it to contain %q", c.path, c.err, got, c.wantSubstr)
		}
	}
}

// A 423 means two things. To a write, somebody has the file open (files_lock).
// To a download, the server itself had the file locked for a moment: nobody is
// holding it, so the reason must not say that somebody is. Five downloads
// refused during a first sync read that way (Deck #950).
func TestA423ToADownloadDoesNotBlameSomeoneElse(t *testing.T) {
	locked := func(op, path string) error {
		return &transport.StatusError{Op: op, Path: path, Code: 423, Status: "423 Locked"}
	}
	for _, tc := range []struct {
		name     string
		kind     engine.ActionKind
		err      error
		download bool
	}{
		{"download", engine.ActDownload, locked("GET", "Team/Budget.xlsx"), true},
		{"download, wrapped", engine.ActDownload, fmt.Errorf("download Team/Budget.xlsx: %w", locked("GET", "Team/Budget.xlsx")), true},
		{"download, untyped", engine.ActDownload, errors.New(`GET "Team/Budget.xlsx": server returned 423 Locked: `), true},
		{"the download half of a conflict", engine.ActConflict, locked("GET", "Team/Budget.xlsx"), true},
		{"upload", engine.ActUpload, locked("PUT", "Team/Budget.xlsx"), false},
		{"upload chunk", engine.ActUpload, locked("PUT chunk 00045", "Team/Budget.xlsx"), false},
		{"upload, untyped", engine.ActUpload, errors.New(`PUT "Team/Budget.xlsx": server returned 423 Locked: `), false},
		{"server delete", engine.ActDeleteRemote, locked("DELETE", "Team/Budget.xlsx"), false},
		{"server rename", engine.ActMoveRemote, locked("MOVE", "Team/Budget.xlsx"), false},
	} {
		got := humanActionErr(engine.Action{Kind: tc.kind, Path: "Team/Budget.xlsx"}, tc.err)
		if tc.download {
			if got != lockedDownloadMsg || strings.Contains(got, "someone else") {
				t.Errorf("%s: reads %q, want %q", tc.name, got, lockedDownloadMsg)
			}
		} else if !strings.Contains(got, "someone else has this file open") {
			t.Errorf("%s: reads %q, want the someone-else wording", tc.name, got)
		}
	}
}

// TestRecordActionResultDedup checks the failure record is humanised, kept once,
// and cleared on a later success (so it can't spam yet still re-reports if it
// recurs).
func TestRecordActionResultDedup(t *testing.T) {
	e := &Engine{}
	a := engine.Action{Kind: engine.ActUpload, Path: ".Collectives/Home Network"}
	werr := errors.New("507 Insufficient Storage in /.Collectives")

	first := e.recordActionResult(a, werr)
	if !strings.Contains(first, "Collectives") {
		t.Fatalf("first failure should return a human .Collectives message, got %q", first)
	}
	// Recording the same failure again keeps a single tracked entry (deduped).
	_ = e.recordActionResult(a, werr)
	if n := e.trackedFailures(); n != 1 {
		t.Errorf("expected 1 tracked failure after a repeat, got %d", n)
	}
	// A later success for the same path clears the record.
	if got := e.recordActionResult(a, nil); got != "" {
		t.Errorf("success should return an empty message, got %q", got)
	}
	if n := e.trackedFailures(); n != 0 {
		t.Errorf("success should clear the tracked failure, got %d", n)
	}
}

// trackedFailures is a test helper for the size of the dedupe map.
func (e *Engine) trackedFailures() int {
	e.failMu.Lock()
	defer e.failMu.Unlock()
	return len(e.lastFail)
}
