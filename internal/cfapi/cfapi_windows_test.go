//go:build windows

package cfapi

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRelFromNormalized(t *testing.T) {
	cases := []struct {
		root, normalized, want string
	}{
		// NormalizedPath is volume-relative (no drive letter).
		{`C:\Users\alice\Root`, `\Users\alice\Root\sub\x`, "sub/x"},
		{`C:\Users\alice\Root`, `\Users\alice\Root`, ""},
		{`C:\Users\alice\Root`, `\Users\alice\Root\one`, "one"},
		// Case differences between the registration and the callback path.
		{`C:\Users\alice\Root`, `\users\alice\root\Sub`, "Sub"},
		// Deep nesting keeps forward slashes.
		{`E:\Cloud`, `\Cloud\a\b\c`, "a/b/c"},
	}
	for _, c := range cases {
		if got := relFromNormalized(c.root, c.normalized); got != c.want {
			t.Errorf("relFromNormalized(%q, %q) = %q, want %q", c.root, c.normalized, got, c.want)
		}
	}
}

func TestToFiletime(t *testing.T) {
	// 1601-01-01 epoch offset: Unix epoch must land exactly on the known constant.
	if got := toFiletime(time.Unix(0, 0)); got != 116444736000000000 {
		t.Errorf("toFiletime(unix epoch) = %d", got)
	}
	// Zero time must not produce a zero FILETIME (Explorer renders that as 1601).
	if got := toFiletime(time.Time{}); got <= 116444736000000000 {
		t.Errorf("toFiletime(zero) = %d, want a current timestamp", got)
	}
}

func TestBuildPlaceholders(t *testing.T) {
	items := []PlaceholderInfo{
		{Name: "file.txt", Size: 42, ModTime: time.Now(), Identity: []byte("remote/file.txt")},
		{Name: "dir", IsDir: true, ModTime: time.Now(), Identity: []byte("remote/dir")},
		{Name: "noid.txt", Size: 1, ModTime: time.Now()},
	}
	arr, names, ids, err := buildPlaceholders(items)
	if err != nil {
		t.Fatal(err)
	}
	if len(arr) != 3 || len(names) != 3 || len(ids) != 3 {
		t.Fatalf("lengths: %d/%d/%d", len(arr), len(names), len(ids))
	}

	// A file is created in-sync; its attrs/size must carry through.
	if arr[0].Flags != cfPlaceholderCreateFlagMarkInSync {
		t.Errorf("file flags = %#x, want MARK_IN_SYNC", arr[0].Flags)
	}
	if arr[0].FsMetadata.BasicInfo.FileAttributes != fileAttrNormal {
		t.Errorf("file attrs = %#x", arr[0].FsMetadata.BasicInfo.FileAttributes)
	}
	if arr[0].FsMetadata.FileSize != 42 {
		t.Errorf("file size = %d", arr[0].FsMetadata.FileSize)
	}
	if arr[0].FileIdentityLength != uint32(len("remote/file.txt")) {
		t.Errorf("identity length = %d", arr[0].FileIdentityLength)
	}

	// A directory must NOT be in-sync (not-in-sync is what triggers lazy
	// FETCH_PLACEHOLDERS population when it's first opened).
	if arr[1].Flags != cfCreateFlagNone {
		t.Errorf("dir flags = %#x, want none (lazy population)", arr[1].Flags)
	}
	if arr[1].FsMetadata.BasicInfo.FileAttributes != fileAttrDirectory {
		t.Errorf("dir attrs = %#x", arr[1].FsMetadata.BasicInfo.FileAttributes)
	}

	// No identity → no pointer (the OS rejects a non-zero length with nil ptr).
	if arr[2].FileIdentity != 0 || arr[2].FileIdentityLength != 0 {
		t.Error("empty identity produced a non-zero identity field")
	}
}

func TestShellRootID(t *testing.T) {
	a, err := shellRootID(`C:\Users\X\CloudA`)
	if err != nil {
		t.Fatal(err)
	}
	b, err := shellRootID(`C:\Users\X\CloudB`)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("distinct paths produced the same shell root id (mounts would collide)")
	}
	aAgain, _ := shellRootID(`c:\users\x\clouda`)
	if a != aAgain {
		t.Error("case-insensitive path produced a different id (re-registration would duplicate)")
	}
}

func TestDirEmpty(t *testing.T) {
	dir := t.TempDir()
	if !dirEmpty(dir) {
		t.Error("fresh temp dir reported non-empty")
	}
	if err := os.WriteFile(filepath.Join(dir, "f"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if dirEmpty(dir) {
		t.Error("dir with a file reported empty")
	}
	if dirEmpty(filepath.Join(dir, "missing")) {
		t.Error("unreadable path must be treated as non-empty (no spurious seeding)")
	}
}

// TestLiveRoundTrip exercises the real Cloud Files driver: register a sync
// root, create placeholders, classify them, convert a fresh file, and purge.
// It mutates HKCU SyncRootManager and talks to cldflt, so it only runs when
// explicitly requested:
//
//	NIMBO_CFAPI_LIVE=1 go test ./internal/cfapi/
func TestLiveRoundTrip(t *testing.T) {
	if os.Getenv("NIMBO_CFAPI_LIVE") == "" {
		t.Skip("set NIMBO_CFAPI_LIVE=1 to run against the real Cloud Files API")
	}
	if !Supported() {
		t.Skip("Cloud Files API not available")
	}
	root := filepath.Join(t.TempDir(), "liveroot")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := Purge(root); err != nil {
			t.Errorf("purge: %v", err)
		}
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Errorf("purge left the root behind")
		}
	})

	hydrate := func(identity []byte, offset, length int64) ([]byte, error) {
		return make([]byte, length), nil
	}
	connKey, err := Mount(root, "NimboTest", "", hydrate, func(rel string) []PlaceholderInfo {
		return nil
	})
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	defer Unmount(root, connKey)

	// Placeholders: an in-sync file and a lazy directory.
	items := []PlaceholderInfo{
		{Name: "ph.txt", Size: 8, ModTime: time.Now(), Identity: []byte("remote/ph.txt")},
		{Name: "sub", IsDir: true, ModTime: time.Now(), Identity: []byte("remote/sub")},
	}
	if err := CreatePlaceholders(root, items); err != nil {
		t.Fatalf("CreatePlaceholders: %v", err)
	}

	// A clean in-sync placeholder must NOT need an upload.
	ch, err := Inspect(filepath.Join(root, "ph.txt"))
	if err != nil {
		t.Fatalf("Inspect placeholder: %v", err)
	}
	if ch.IsDir || ch.NeedsUpload {
		t.Errorf("placeholder file classified as %+v, want clean file", ch)
	}

	// Our lazy directory placeholders are not-in-sync BY DESIGN; that must not
	// read as "needs upload".
	ch, err = Inspect(filepath.Join(root, "sub"))
	if err != nil {
		t.Fatalf("Inspect dir: %v", err)
	}
	if !ch.IsDir || ch.NeedsUpload {
		t.Errorf("lazy dir classified as %+v, want clean dir", ch)
	}

	// A brand-new regular file needs an upload; after MarkInSync it must not.
	fresh := filepath.Join(root, "fresh.txt")
	if err := os.WriteFile(fresh, []byte("user data"), 0o644); err != nil {
		t.Fatal(err)
	}
	ch, err = Inspect(fresh)
	if err != nil {
		t.Fatalf("Inspect fresh: %v", err)
	}
	if !ch.NeedsUpload {
		t.Error("fresh user file not flagged for upload")
	}
	if err := MarkInSync(fresh, []byte("remote/fresh.txt")); err != nil {
		t.Fatalf("MarkInSync: %v", err)
	}
	ch, err = Inspect(fresh)
	if err != nil {
		t.Fatalf("Inspect converted: %v", err)
	}
	if ch.NeedsUpload {
		t.Error("converted placeholder still flagged for upload")
	}
}
