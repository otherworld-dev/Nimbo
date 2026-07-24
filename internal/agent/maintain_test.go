package agent

import (
	"path/filepath"
	"testing"

	"github.com/otherworld/nimbo/internal/engine"
	"github.com/otherworld/nimbo/internal/state"
)

func maintTestStore(t *testing.T) *state.Store {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"), "acct", false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// The post-pass maintenance must stamp re-listed dirs with their scan-time etag
// (so the ETag prune converges instead of re-listing every ever-changed dir on
// every pass), but never stamp — and actively dirty — the ancestor chain of a
// failed or conflicted path, so unfinished subtrees keep being re-scanned.
func TestMaintainDirBaselines(t *testing.T) {
	st := maintTestStore(t)
	const pk = "P"

	base := map[string]engine.BaselineState{
		"stale":       {Path: "stale", IsDir: true, RemoteETag: "old"},
		"stale/inner": {Path: "stale/inner", IsDir: true, RemoteETag: "old2"},
		"fresh":       {Path: "fresh", IsDir: true, RemoteETag: "same"},
		"bad":         {Path: "bad", IsDir: true, RemoteETag: "old3", RemoteFileID: "fid-bad"},
		"bad/sub":     {Path: "bad/sub", IsDir: true, RemoteETag: "old4"},
	}
	for _, b := range base {
		if err := st.UpsertBaseline(pk, b); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	remote := map[string]engine.RemoteState{
		"stale":       {Path: "stale", IsDir: true, ETag: "NEW", FileID: "fid-stale"},
		"stale/inner": {Path: "stale/inner", IsDir: true, ETag: "NEW2"},
		"fresh":       {Path: "fresh", IsDir: true, ETag: "same"},
		"bad":         {Path: "bad", IsDir: true, ETag: "NEW3"},
		"bad/sub":     {Path: "bad/sub", IsDir: true, ETag: "NEW4"},
		"newdir":      {Path: "newdir", IsDir: true, ETag: "N1", FileID: "fid-new"},
		"a-file.txt":  {Path: "a-file.txt", ETag: "fe"}, // files never produce dir rows
	}

	// One failure deep under "bad" poisons bad and bad/sub, nothing else.
	maintainDirBaselines(st, pk, base, remote, []string{"bad/sub/file.doc"})

	got, err := st.LoadBaseline(pk)
	if err != nil {
		t.Fatalf("LoadBaseline: %v", err)
	}
	if got["stale"].RemoteETag != "NEW" || got["stale"].RemoteFileID != "fid-stale" {
		t.Errorf("stale dir not stamped: %+v", got["stale"])
	}
	if got["stale/inner"].RemoteETag != "NEW2" {
		t.Errorf("nested stale dir not stamped: %+v", got["stale/inner"])
	}
	if got["fresh"].RemoteETag != "same" {
		t.Errorf("fresh dir should be untouched: %+v", got["fresh"])
	}
	if got["newdir"].RemoteETag != "N1" || !got["newdir"].IsDir {
		t.Errorf("newly listed dir (no base row) not stamped: %+v", got["newdir"])
	}
	if _, ok := got["a-file.txt"]; ok {
		t.Error("maintenance must never write file rows")
	}
	// The poisoned chain is dirtied (empty etag = always re-listed), never stamped.
	if got["bad"].RemoteETag != "" || got["bad/sub"].RemoteETag != "" {
		t.Errorf("failed subtree chain not dirtied: bad=%+v bad/sub=%+v", got["bad"], got["bad/sub"])
	}
	if !got["bad"].IsDir {
		t.Errorf("dirty marker lost IsDir: %+v", got["bad"])
	}
}

// nil base (a stat-built remote map, e.g. SyncPaths) must never stamp — only
// dirty the chains of failed paths.
func TestMaintainDirBaselinesNilBase(t *testing.T) {
	st := maintTestStore(t)
	const pk = "P"
	if err := st.UpsertBaseline(pk, engine.BaselineState{Path: "d", IsDir: true, RemoteETag: "stale", RemoteFileID: "fid"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	remote := map[string]engine.RemoteState{
		"d": {Path: "d", IsDir: true, ETag: "NEW"},
	}

	maintainDirBaselines(st, pk, nil, remote, nil)
	got, _ := st.LoadBaseline(pk)
	if got["d"].RemoteETag != "stale" {
		t.Errorf("nil base must not stamp: %+v", got["d"])
	}

	maintainDirBaselines(st, pk, nil, remote, []string{"d/file"})
	got, _ = st.LoadBaseline(pk)
	if got["d"].RemoteETag != "" || got["d"].RemoteFileID != "" && got["d"].RemoteFileID != "fid" {
		t.Errorf("failure under d should dirty it: %+v", got["d"])
	}
}

// A conflict path poisons its chain exactly like a failure — a pruned ancestor
// would stop the conflict from being re-detected.
func TestMaintainDirBaselinesConflictPoisons(t *testing.T) {
	st := maintTestStore(t)
	const pk = "P"
	base := map[string]engine.BaselineState{
		"docs": {Path: "docs", IsDir: true, RemoteETag: "old"},
	}
	if err := st.UpsertBaseline(pk, base["docs"]); err != nil {
		t.Fatalf("seed: %v", err)
	}
	remote := map[string]engine.RemoteState{
		"docs": {Path: "docs", IsDir: true, ETag: "NEW"},
	}
	maintainDirBaselines(st, pk, base, remote, []string{"docs/report.docx"})
	got, _ := st.LoadBaseline(pk)
	if got["docs"].RemoteETag != "" {
		t.Errorf("conflicted subtree must stay dirty, got etag %q", got["docs"].RemoteETag)
	}
}

func TestDirParent(t *testing.T) {
	for in, want := range map[string]string{
		"a/b/c": "a/b", "a/b": "a", "a": "", "": "",
	} {
		if got := dirParent(in); got != want {
			t.Errorf("dirParent(%q) = %q, want %q", in, got, want)
		}
	}
}
