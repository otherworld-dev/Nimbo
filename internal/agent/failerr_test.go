package agent

import (
	"errors"
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
