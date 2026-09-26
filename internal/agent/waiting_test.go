package agent

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/otherworld/nimbo/internal/activity"
)

// A file held for a program was recorded like a failed upload: a red "failed"
// row in the flyout for a hold that is working as meant, while the status line
// said Waiting (Deck #714). It is one neutral "waiting" row per wait, not a row
// every pass that meets it again, and it clears any earlier error for the file.
func TestAFileWaitingOnAProgramIsOneWaitingRowNotAFailure(t *testing.T) {
	e := &Engine{recorder: activity.New()}
	dir := `E:\Nextcloud`
	abs := filepath.Join(dir, "To Sort", "archive.pst")
	e.recorder.Add(activity.Event{Local: dir, Path: "To Sort/archive.pst", Kind: "upload", Err: "torn"})
	e.noteWaiting(dir, abs, "To Sort/archive.pst")
	e.noteWaiting(dir, abs, "To Sort/archive.pst") // the next pass meets it again

	waiting := 0
	for _, ev := range e.recorder.Recent() {
		if ev.Kind == "waiting" {
			waiting++
			if ev.Err != "" {
				t.Errorf("waiting row carries an error: %q", ev.Err)
			}
		}
	}
	if waiting != 1 {
		t.Errorf("%d waiting rows, want 1", waiting)
	}
	if n := len(e.recorder.Errors()); n != 0 {
		t.Errorf("%d unresolved errors left for a file that is only waiting", n)
	}
	e.status("Up to date")
	if !strings.HasPrefix(e.lastStatus, "Waiting") {
		t.Errorf("status = %q while the file waits", e.lastStatus)
	}
}
