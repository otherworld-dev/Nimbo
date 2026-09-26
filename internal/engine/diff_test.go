package engine

import (
	"github.com/otherworld/nimbo/internal/transport"
	"sort"
	"testing"
	"time"
)

// helpers to build state maps concisely.
var t0 = time.Unix(1700000000, 0)

func base(path string, etag string, size int64, mt time.Time) BaselineState {
	return BaselineState{Path: path, RemoteETag: etag, LocalSize: size, LocalMTimeNanos: mt.UnixNano()}
}
func remote(path, etag string, size int64) RemoteState {
	return RemoteState{Path: path, ETag: etag, Size: size}
}
func local(path string, size int64, mt time.Time) LocalState {
	return LocalState{Path: path, Size: size, MTime: mt}
}

// find returns the action for a path, or ActNoop if absent.
func find(actions []Action, path string) Action {
	for _, a := range actions {
		if a.Path == path {
			return a
		}
	}
	return Action{Kind: ActNoop, Path: path}
}

func TestDiff_CoreCases(t *testing.T) {
	t1 := t0.Add(time.Hour) // a later mtime

	tests := []struct {
		name string
		base map[string]BaselineState
		rem  map[string]RemoteState
		loc  map[string]LocalState
		path string
		want ActionKind
	}{
		{
			name: "new on remote only -> download",
			base: nil,
			rem:  map[string]RemoteState{"a.txt": remote("a.txt", "e1", 10)},
			loc:  nil,
			path: "a.txt", want: ActDownload,
		},
		{
			name: "new locally only -> upload",
			base: nil,
			rem:  nil,
			loc:  map[string]LocalState{"a.txt": local("a.txt", 10, t0)},
			path: "a.txt", want: ActUpload,
		},
		{
			name: "unchanged both sides -> noop",
			base: map[string]BaselineState{"a.txt": base("a.txt", "e1", 10, t0)},
			rem:  map[string]RemoteState{"a.txt": remote("a.txt", "e1", 10)},
			loc:  map[string]LocalState{"a.txt": local("a.txt", 10, t0)},
			path: "a.txt", want: ActNoop,
		},
		{
			name: "remote changed only -> download",
			base: map[string]BaselineState{"a.txt": base("a.txt", "e1", 10, t0)},
			rem:  map[string]RemoteState{"a.txt": remote("a.txt", "e2", 12)},
			loc:  map[string]LocalState{"a.txt": local("a.txt", 10, t0)},
			path: "a.txt", want: ActDownload,
		},
		{
			name: "local changed only -> upload",
			base: map[string]BaselineState{"a.txt": base("a.txt", "e1", 10, t0)},
			rem:  map[string]RemoteState{"a.txt": remote("a.txt", "e1", 10)},
			loc:  map[string]LocalState{"a.txt": local("a.txt", 15, t1)},
			path: "a.txt", want: ActUpload,
		},
		{
			name: "both changed -> conflict",
			base: map[string]BaselineState{"a.txt": base("a.txt", "e1", 10, t0)},
			rem:  map[string]RemoteState{"a.txt": remote("a.txt", "e2", 20)},
			loc:  map[string]LocalState{"a.txt": local("a.txt", 15, t1)},
			path: "a.txt", want: ActConflict,
		},
		{
			// Both sides created the same path with no common ancestor: must be a
			// conflict, never a silent overwrite of one side (the both-new race).
			name: "appeared on both sides, no baseline, different size -> conflict",
			base: nil,
			rem:  map[string]RemoteState{"a.txt": remote("a.txt", "e1", 20)},
			loc:  map[string]LocalState{"a.txt": local("a.txt", 10, t1)},
			path: "a.txt", want: ActConflict,
		},
		{
			name: "appeared on both sides, no baseline, same size -> conflict",
			base: nil,
			rem:  map[string]RemoteState{"a.txt": remote("a.txt", "e1", 10)},
			loc:  map[string]LocalState{"a.txt": local("a.txt", 10, t1)},
			path: "a.txt", want: ActConflict,
		},
		{
			name: "deleted locally, remote unchanged -> delete remote",
			base: map[string]BaselineState{"a.txt": base("a.txt", "e1", 10, t0)},
			rem:  map[string]RemoteState{"a.txt": remote("a.txt", "e1", 10)},
			loc:  nil,
			path: "a.txt", want: ActDeleteRemote,
		},
		{
			name: "deleted remotely, local unchanged -> delete local",
			base: map[string]BaselineState{"a.txt": base("a.txt", "e1", 10, t0)},
			rem:  nil,
			loc:  map[string]LocalState{"a.txt": local("a.txt", 10, t0)},
			path: "a.txt", want: ActDeleteLocal,
		},
		{
			name: "deleted locally but modified remotely -> conflict",
			base: map[string]BaselineState{"a.txt": base("a.txt", "e1", 10, t0)},
			rem:  map[string]RemoteState{"a.txt": remote("a.txt", "e2", 10)},
			loc:  nil,
			path: "a.txt", want: ActConflict,
		},
		{
			name: "deleted remotely but modified locally -> conflict",
			base: map[string]BaselineState{"a.txt": base("a.txt", "e1", 10, t0)},
			rem:  nil,
			loc:  map[string]LocalState{"a.txt": local("a.txt", 99, t1)},
			path: "a.txt", want: ActConflict,
		},
		{
			name: "deleted on both sides -> noop",
			base: map[string]BaselineState{"a.txt": base("a.txt", "e1", 10, t0)},
			rem:  nil,
			loc:  nil,
			path: "a.txt", want: ActNoop,
		},
		{
			name: "new remote dir -> mkdir local",
			base: nil,
			rem:  map[string]RemoteState{"d": {Path: "d", IsDir: true, ETag: "e1"}},
			loc:  nil,
			path: "d", want: ActCreateLocalDir,
		},
		{
			name: "new local dir -> mkdir remote",
			base: nil,
			rem:  nil,
			loc:  map[string]LocalState{"d": {Path: "d", IsDir: true}},
			path: "d", want: ActCreateRemoteDir,
		},
		{
			name: "dir both sides -> noop",
			base: nil,
			rem:  map[string]RemoteState{"d": {Path: "d", IsDir: true, ETag: "e1"}},
			loc:  map[string]LocalState{"d": {Path: "d", IsDir: true}},
			path: "d", want: ActNoop,
		},
		{
			name: "type mismatch dir vs file -> conflict",
			base: nil,
			rem:  map[string]RemoteState{"x": {Path: "x", IsDir: true, ETag: "e1"}},
			loc:  map[string]LocalState{"x": local("x", 5, t0)},
			path: "x", want: ActConflict,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := find(Diff(tc.base, tc.rem, tc.loc), tc.path)
			if got.Kind != tc.want {
				t.Errorf("path %q: got %s (%q), want %s", tc.path, got.Kind, got.Reason, tc.want)
			}
		})
	}
}

// TestDiff_NoopsExcluded verifies that unchanged paths never appear in output.
func TestDiff_NoopsExcluded(t *testing.T) {
	b := map[string]BaselineState{"a.txt": base("a.txt", "e1", 10, t0)}
	r := map[string]RemoteState{"a.txt": remote("a.txt", "e1", 10)}
	l := map[string]LocalState{"a.txt": local("a.txt", 10, t0)}
	if got := Diff(b, r, l); len(got) != 0 {
		t.Errorf("expected no actions, got %v", got)
	}
}

// Dead rows — baseline entries present on neither side — must be reported for
// pruning; anything alive on either side must not.
func TestDeadBaselinePaths(t *testing.T) {
	base := map[string]BaselineState{
		"gone":          {Path: "gone", IsDir: true, RemoteETag: "e"},
		"gone/file.txt": {Path: "gone/file.txt", RemoteETag: "e"},
		"remote-only":   {Path: "remote-only", RemoteETag: "e"},
		"local-only":    {Path: "local-only", RemoteETag: "e"},
		"alive":         {Path: "alive", RemoteETag: "e"},
	}
	remote := map[string]RemoteState{
		"remote-only": {Path: "remote-only", ETag: "e"},
		"alive":       {Path: "alive", ETag: "e"},
	}
	local := map[string]LocalState{
		"local-only": {Path: "local-only"},
		"alive":      {Path: "alive"},
	}
	dead := DeadBaselinePaths(base, remote, local)
	sort.Strings(dead)
	want := []string{"gone", "gone/file.txt"}
	if len(dead) != len(want) || dead[0] != want[0] || dead[1] != want[1] {
		t.Fatalf("DeadBaselinePaths = %v, want %v", dead, want)
	}
	if got := DeadBaselinePaths(nil, remote, local); len(got) != 0 {
		t.Fatalf("empty base should yield none, got %v", got)
	}
}

// A metadata-only ETag bump - which is what taking or releasing a files_lock
// lock produces - must not read as a content change.
//
// Two failures without this: every peer re-downloads the file twice per lock
// cycle, and a peer that ALSO edited locally gets a conflicted copy out of a
// file nobody changed on the server.
func TestDiffIgnoresMetadataOnlyEtagBump(t *testing.T) {
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	base := map[string]BaselineState{
		"a.xlsx": {Path: "a.xlsx", RemoteETag: "old", ContentSHA1: sha, LocalSize: 5, LocalMTimeNanos: 1000},
	}
	remote := map[string]RemoteState{
		"a.xlsx": {Path: "a.xlsx", ETag: "new-after-lock", SHA1: sha, Size: 5},
	}

	// Local untouched: nothing to do at all.
	local := map[string]LocalState{
		"a.xlsx": {Path: "a.xlsx", Size: 5, MTime: time.Unix(0, 1000)},
	}
	for _, a := range Diff(base, remote, local) {
		if a.Kind != ActNoop {
			t.Errorf("unchanged file produced %v (%s); want nothing", a.Kind, a.Reason)
		}
	}

	// Local edited: that is a plain upload, NOT a conflict.
	local["a.xlsx"] = LocalState{Path: "a.xlsx", Size: 9, MTime: time.Unix(0, 2000)}
	var kinds []ActionKind
	for _, a := range Diff(base, remote, local) {
		if a.Kind != ActNoop {
			kinds = append(kinds, a.Kind)
		}
	}
	if len(kinds) != 1 || kinds[0] != ActUpload {
		t.Errorf("got %v, want a single upload — the server's bytes never changed", kinds)
	}
}

// The override is conservative: with no checksum to compare, the ETag is still
// trusted, so a real remote edit is never mistaken for metadata.
func TestDiffTrustsEtagWithoutChecksums(t *testing.T) {
	base := map[string]BaselineState{
		"a.xlsx": {Path: "a.xlsx", RemoteETag: "old", LocalSize: 5, LocalMTimeNanos: 1000},
	}
	remote := map[string]RemoteState{"a.xlsx": {Path: "a.xlsx", ETag: "new", Size: 6}}
	local := map[string]LocalState{"a.xlsx": {Path: "a.xlsx", Size: 5, MTime: time.Unix(0, 1000)}}

	var kinds []ActionKind
	for _, a := range Diff(base, remote, local) {
		if a.Kind != ActNoop {
			kinds = append(kinds, a.Kind)
		}
	}
	if len(kinds) != 1 || kinds[0] != ActDownload {
		t.Errorf("got %v, want a download", kinds)
	}
}

// A genuine remote edit still conflicts with a genuine local edit.
func TestDiffStillConflictsOnRealChange(t *testing.T) {
	base := map[string]BaselineState{
		"a.xlsx": {Path: "a.xlsx", RemoteETag: "old", ContentSHA1: "1111111111111111111111111111111111111111", LocalSize: 5, LocalMTimeNanos: 1000},
	}
	remote := map[string]RemoteState{
		"a.xlsx": {Path: "a.xlsx", ETag: "new", SHA1: "2222222222222222222222222222222222222222", Size: 7},
	}
	local := map[string]LocalState{"a.xlsx": {Path: "a.xlsx", Size: 9, MTime: time.Unix(0, 2000)}}

	var kinds []ActionKind
	for _, a := range Diff(base, remote, local) {
		if a.Kind != ActNoop {
			kinds = append(kinds, a.Kind)
		}
	}
	if len(kinds) != 1 || kinds[0] != ActConflict {
		t.Errorf("got %v, want a conflict", kinds)
	}
}

// Most files carry no oc:checksums (anything uploaded through the browser, for
// one), so the SHA1 rule above cannot see a files_lock bump on them. The
// content key can: a lock and an unlock leave size, mtime and nc:upload_time
// alone (GitHub #7).
func TestDiffIgnoresLockBumpByContentKey(t *testing.T) {
	mt := time.Unix(1790160000, 0)
	key := transport.ContentKey(5, mt, 1790160705)
	base := map[string]BaselineState{
		"a.xlsx": {Path: "a.xlsx", RemoteETag: "old", ContentKey: key, LocalSize: 5, LocalMTimeNanos: 1000},
	}
	remote := map[string]RemoteState{
		"a.xlsx": {Path: "a.xlsx", ETag: "new-after-lock", Size: 5, LastModified: mt, UploadTime: 1790160705},
	}
	local := map[string]LocalState{"a.xlsx": {Path: "a.xlsx", Size: 5, MTime: time.Unix(0, 1000)}}
	for _, a := range Diff(base, remote, local) {
		if a.Kind != ActNoop {
			t.Errorf("unchanged file produced %v (%s); want nothing", a.Kind, a.Reason)
		}
	}

	local["a.xlsx"] = LocalState{Path: "a.xlsx", Size: 9, MTime: time.Unix(0, 2000)}
	var kinds []ActionKind
	for _, a := range Diff(base, remote, local) {
		if a.Kind != ActNoop {
			kinds = append(kinds, a.Kind)
		}
	}
	if len(kinds) != 1 || kinds[0] != ActUpload {
		t.Errorf("got %v, want a single upload, the server's version never changed", kinds)
	}
}

// A new upload of the same size and mtime has a new upload time, so the key
// does not hide it.
func TestDiffContentKeyStillSeesReupload(t *testing.T) {
	mt := time.Unix(1790160000, 0)
	base := map[string]BaselineState{
		"a.xlsx": {Path: "a.xlsx", RemoteETag: "old", ContentKey: transport.ContentKey(5, mt, 1790160705), LocalSize: 5, LocalMTimeNanos: 1000},
	}
	remote := map[string]RemoteState{
		"a.xlsx": {Path: "a.xlsx", ETag: "new", Size: 5, LastModified: mt, UploadTime: 1790160846},
	}
	local := map[string]LocalState{"a.xlsx": {Path: "a.xlsx", Size: 5, MTime: time.Unix(0, 1000)}}
	var kinds []ActionKind
	for _, a := range Diff(base, remote, local) {
		if a.Kind != ActNoop {
			kinds = append(kinds, a.Kind)
		}
	}
	if len(kinds) != 1 || kinds[0] != ActDownload {
		t.Errorf("got %v, want a download", kinds)
	}
}
