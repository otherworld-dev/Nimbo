//go:build windows

package vfs

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/otherworld/nimbo/internal/engine"
)

var adoptT0 = time.Unix(1700000000, 0)

// adoptTree builds a local folder and returns its path. Each spec is
// "rel:size:mtimeOffsetSeconds"; a trailing ":stub" marks the file offline
// (FILE_ATTRIBUTE_OFFLINE), standing in for another client's dehydrated
// placeholder — the one dehydration marker a test can set directly.
func adoptTree(t *testing.T, specs ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, s := range specs {
		parts := strings.Split(s, ":")
		rel, size, off := parts[0], 0, 0
		if len(parts) > 1 && parts[1] != "" {
			size = atoiOrFail(t, parts[1])
		}
		if len(parts) > 2 && parts[2] != "" {
			off = atoiOrFail(t, parts[2])
		}
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := adoptT0.Add(time.Duration(off) * time.Second)
		if err := os.Chtimes(full, mt, mt); err != nil {
			t.Fatal(err)
		}
		if len(parts) > 3 && parts[3] == "stub" {
			markOffline(t, full)
		}
	}
	return dir
}

func atoiOrFail(t *testing.T, s string) int {
	t.Helper()
	n := 0
	neg := false
	for i, c := range s {
		if i == 0 && c == '-' {
			neg = true
			continue
		}
		if c < '0' || c > '9' {
			t.Fatalf("bad number %q", s)
		}
		n = n*10 + int(c-'0')
	}
	if neg {
		return -n
	}
	return n
}

func markOffline(t *testing.T, path string) {
	t.Helper()
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	attrs, err := syscall.GetFileAttributes(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.SetFileAttributes(p, attrs|0x00001000); err != nil {
		t.Skipf("cannot set FILE_ATTRIBUTE_OFFLINE here: %v", err)
	}
}

func remoteFile(size int64, offSeconds int) engine.RemoteState {
	return engine.RemoteState{Size: size, LastModified: adoptT0.Add(time.Duration(offSeconds) * time.Second)}
}

// actionFor finds a plan entry by path.
func actionFor(t *testing.T, p Plan, rel string) Action {
	t.Helper()
	for _, e := range p.Entries {
		if e.Rel == rel {
			return e.Action
		}
	}
	t.Fatalf("no plan entry for %q (have %v)", rel, planPaths(p))
	return 0
}

func planPaths(p Plan) []string {
	var out []string
	for _, e := range p.Entries {
		out = append(out, e.Rel)
	}
	sort.Strings(out)
	return out
}

func TestAdoptScanClassifies(t *testing.T) {
	dir := adoptTree(t,
		"match.txt:10:0",      // identical to server
		"near.txt:10:1",       // 1s adrift — still a match
		"differs.txt:99:0",    // size differs
		"localonly.txt:5:0",   // not on the server
		"sub/nested.txt:20:0", // identical, nested
		"stub.txt:10:0:stub",  // another client's dehydrated placeholder
		"Thumbs.db:4:0",       // junk, must be ignored
	)
	remote := map[string]engine.RemoteState{
		"match.txt":      remoteFile(10, 0),
		"near.txt":       remoteFile(10, 0),
		"differs.txt":    remoteFile(10, 0),
		"sub/nested.txt": remoteFile(20, 0),
		"stub.txt":       remoteFile(10, 0),
		"server-only.md": remoteFile(7, 0),
	}
	plan, err := Scan(dir, remote, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := actionFor(t, plan, "match.txt"); got != ActionKeep {
		t.Errorf("match.txt: got %v, want Keep", got)
	}
	if got := actionFor(t, plan, "near.txt"); got != ActionKeep {
		t.Errorf("near.txt (1s adrift): got %v, want Keep", got)
	}
	if got := actionFor(t, plan, "sub/nested.txt"); got != ActionKeep {
		t.Errorf("sub/nested.txt: got %v, want Keep", got)
	}
	if got := actionFor(t, plan, "differs.txt"); got != ActionConflict {
		t.Errorf("differs.txt: got %v, want Conflict", got)
	}
	if got := actionFor(t, plan, "localonly.txt"); got != ActionUpload {
		t.Errorf("localonly.txt: got %v, want Upload", got)
	}
	// A dehydrated stub holds no content, so it must never be kept even though
	// its size and mtime match the server exactly.
	if got := actionFor(t, plan, "stub.txt"); got != ActionReplace {
		t.Errorf("stub.txt: got %v, want Replace", got)
	}
	for _, e := range plan.Entries {
		if e.Rel == "Thumbs.db" {
			t.Error("junk file must not appear in the plan")
		}
		if e.Rel == "server-only.md" {
			t.Error("remote-only entries belong to reconcile, not the plan")
		}
	}
}

func TestAdoptPlanSummary(t *testing.T) {
	dir := adoptTree(t, "a.txt:10:0", "b.txt:3:0", "c.txt:7:0")
	remote := map[string]engine.RemoteState{"a.txt": remoteFile(10, 0)}
	plan, err := Scan(dir, remote, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	counts := plan.Counts()
	if counts[ActionKeep] != 1 {
		t.Errorf("Keep count = %d, want 1", counts[ActionKeep])
	}
	if counts[ActionUpload] != 2 {
		t.Errorf("Upload count = %d, want 2", counts[ActionUpload])
	}
	// The summary must state the upload cost before the user commits.
	if got := plan.UploadBytes(); got != 10 {
		t.Errorf("UploadBytes = %d, want 10 (3+7)", got)
	}
}

func TestAdoptScanEmptyRemoteUploadsEverything(t *testing.T) {
	dir := adoptTree(t, "a.txt:10:0", "sub/b.txt:5:0")
	plan, err := Scan(dir, map[string]engine.RemoteState{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range plan.Entries {
		if e.Action != ActionUpload {
			t.Errorf("%s: got %v, want Upload when the server has nothing", e.Rel, e.Action)
		}
	}
}

// The adopt scan must respect the sync ignore rules: live mode never uploaded
// node_modules/.git etc., so classifying them as "upload" offers to push
// gigabytes of dev-tree junk to the server (live incident: 15 GB / ~260k files
// of ignored trees in the upload bucket). Ignored paths stay plain local files,
// absent from the plan entirely.
func TestAdoptScanRespectsIgnores(t *testing.T) {
	dir := adoptTree(t,
		"keep.txt:10:0",
		"node_modules/dep/index.js:5:0",
		"proj/.git/HEAD:3:0",
	)
	remote := map[string]engine.RemoteState{"keep.txt": remoteFile(10, 0)}
	skip := func(rel string) bool {
		return strings.HasPrefix(rel, "node_modules/") || strings.Contains(rel, "/.git/") ||
			rel == "node_modules" || strings.HasSuffix(rel, "/.git")
	}
	plan, err := Scan(dir, remote, skip, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := actionFor(t, plan, "keep.txt"); got != ActionKeep {
		t.Errorf("keep.txt: got %v, want Keep", got)
	}
	for _, e := range plan.Entries {
		if strings.Contains(e.Rel, "node_modules") || strings.Contains(e.Rel, ".git") {
			t.Errorf("ignored path %q must not be in the plan", e.Rel)
		}
	}
	if len(plan.Entries) != 1 {
		t.Errorf("plan has %d entries, want 1: %v", len(plan.Entries), planPaths(plan))
	}
}

// Escaped names: a server-forbidden name (e.g. .htaccess) is stored on the
// server under an escaped name (.htaccess.nimboesc). The adopt must classify
// the LOCAL name against the DECODED remote map, but mark identities and
// upload targets with the ESCAPED server name — the live incident uploaded
// literal .htaccess to the server and then 404-looped fetching the escaped one.
func TestAdoptEscapedNames(t *testing.T) {
	dir := adoptTree(t,
		"web/.htaccess:10:0", // on server as web/.htaccess.nimboesc -> Keep
		"api/.htaccess:5:0",  // not on server -> Upload, to the ESCAPED name
	)
	remote := map[string]engine.RemoteState{ // already decoded, as scanAdopt provides
		"web/.htaccess": remoteFile(10, 0),
	}
	remoteName := func(rel string) string {
		if strings.HasSuffix(rel, ".htaccess") {
			return rel + ".nimboesc"
		}
		return rel
	}
	plan, err := Scan(dir, remote, nil, remoteName)
	if err != nil {
		t.Fatal(err)
	}
	if got := actionFor(t, plan, "web/.htaccess"); got != ActionKeep {
		t.Errorf("web/.htaccess: got %v, want Keep", got)
	}
	for _, e := range plan.Entries {
		want := e.Rel + ".nimboesc"
		if e.RemoteRel != want {
			t.Errorf("Entry %q RemoteRel = %q, want %q", e.Rel, e.RemoteRel, want)
		}
	}
}
