package transfer

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/otherworld/nimbo/internal/engine"
	"github.com/otherworld/nimbo/internal/state"
)

// The baseline rows the executor writes must carry the share/mount-root flag
// the scan found, or the baseline could never say "this folder was a share"
// once the share has vanished.
func TestExecutorRecordsMountRoot(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"), "acct", false)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e := &Executor{
		State: st, PairKey: "P", LocalRoot: t.TempDir(),
		Remote: map[string]engine.RemoteState{
			"Team":        {Path: "Team", IsDir: true, ETag: "et", FileID: "1", MountRoot: true},
			"own":         {Path: "own", IsDir: true, ETag: "eo", FileID: "2"},
			"Budget.xlsx": {Path: "Budget.xlsx", ETag: "eb", FileID: "3", MountRoot: true},
		},
	}
	if err := e.makeLocalDir("Team"); err != nil {
		t.Fatal(err)
	}
	if err := e.makeLocalDir("own"); err != nil {
		t.Fatal(err)
	}
	if err := e.saveFileBaseline("Budget.xlsx", FileResult{ETag: "eb", FileID: "3", Size: 9}); err != nil {
		t.Fatal(err)
	}
	// A file the scan never saw (a fresh local upload) gets no flag.
	if err := e.saveFileBaseline("new.txt", FileResult{ETag: "en", Size: 1}); err != nil {
		t.Fatal(err)
	}

	got, err := st.LoadBaseline("P")
	if err != nil {
		t.Fatal(err)
	}
	if !got["Team"].MountRoot || !got["Team"].IsDir || got["Team"].RemoteETag != "et" {
		t.Errorf("share root dir row: %+v", got["Team"])
	}
	if got["own"].MountRoot {
		t.Errorf("own dir row flagged: %+v", got["own"])
	}
	if !got["Budget.xlsx"].MountRoot || got["Budget.xlsx"].LocalSize != 9 {
		t.Errorf("shared file row: %+v", got["Budget.xlsx"])
	}
	if got["new.txt"].MountRoot {
		t.Errorf("unlisted file row flagged: %+v", got["new.txt"])
	}
}

// A synced file's baseline carries the content key of the version the listing
// showed, but only when the saved version IS that one: an upload makes a new
// version the listing never saw, and an old key must not vouch for it.
func TestExecutorRecordsContentKey(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"), "acct", false)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mt := time.Unix(1790160000, 0)
	listed := engine.RemoteState{Path: "d.txt", ETag: "e-listed", Size: 5, LastModified: mt, UploadTime: 1790160705}
	e := &Executor{State: st, PairKey: "P", LocalRoot: t.TempDir(),
		Remote: map[string]engine.RemoteState{"d.txt": listed, "u.txt": {Path: "u.txt", ETag: "e-before", Size: 5, LastModified: mt, UploadTime: 1790160705}}}
	if err := e.saveFileBaseline("d.txt", FileResult{ETag: "e-listed", Size: 5}); err != nil {
		t.Fatal(err)
	}
	if err := e.saveFileBaseline("u.txt", FileResult{ETag: "e-uploaded", Size: 7}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.LoadBaseline("P")
	if want := listed.ContentKey(); got["d.txt"].ContentKey != want || want == "" {
		t.Errorf("downloaded file key = %q, want %q", got["d.txt"].ContentKey, want)
	}
	if got["u.txt"].ContentKey != "" {
		t.Errorf("uploaded file kept the listed version's key %q", got["u.txt"].ContentKey)
	}
}
