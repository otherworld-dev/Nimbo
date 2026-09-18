//go:build windows

package cfapi

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestMetadataOpensDoNotHydrate pins the openForCloud fix (2026-09-14):
// opening a placeholder for metadata-only cfapi calls must never pull its
// data down as a side effect. Before the fix, openForCloud requested
// GENERIC_READ|GENERIC_WRITE, and the Cloud Files filter hydrates a
// dehydrated placeholder the instant it's opened for ANY data access —
// before any cfapi call even runs — so every metadata call here
// (PlaceholderIdentity, SetPinState, UpdateIdentity, Dehydrate) silently
// downloaded an online-only file first just by opening it. The legs below
// cover every openForCloud caller that MUTATES placeholder state —
// RefreshPlaceholder and UpdateIdentityKeepState (flags 0) included, the two
// that had no live coverage on the new handle at all.
// Live-driver test, opt in with NIMBO_CFAPI_LIVE=1.
func TestMetadataOpensDoNotHydrate(t *testing.T) {
	if os.Getenv("NIMBO_CFAPI_LIVE") == "" {
		t.Skip("set NIMBO_CFAPI_LIVE=1 to run against the real Cloud Files API")
	}
	if !Supported() {
		t.Skip("Cloud Files API not available")
	}
	root := filepath.Join(t.TempDir(), "metaroot")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Purge(root) })

	var fetches atomic.Int64
	hydrate := func(identity []byte, offset, length int64) ([]byte, error) {
		fetches.Add(1)
		return make([]byte, length), nil
	}
	list := func(rel string) []PlaceholderInfo { return []PlaceholderInfo{} }
	connKey, err := Mount(root, "NimboMetaTest", "", hydrate, list)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	defer Unmount(root, connKey)

	identity := []byte("remote/meta.bin")
	if err := CreatePlaceholders(root, []PlaceholderInfo{
		{Name: "meta.bin", Size: 4096, ModTime: time.Now(), Identity: identity},
	}); err != nil {
		t.Fatalf("CreatePlaceholders: %v", err)
	}
	file := filepath.Join(root, "meta.bin")

	if _, err := PlaceholderIdentity(file); err != nil {
		t.Fatalf("PlaceholderIdentity: %v", err)
	}
	if err := SetPinState(file, true, false); err != nil {
		t.Fatalf("SetPinState(pinned=true): %v", err)
	}
	if err := SetPinState(file, false, false); err != nil {
		t.Fatalf("SetPinState(pinned=false): %v", err)
	}
	if err := UpdateIdentity(file, identity); err != nil {
		t.Fatalf("UpdateIdentity: %v", err)
	}
	if err := Dehydrate(file); err != nil {
		t.Fatalf("Dehydrate: %v", err)
	}

	time.Sleep(500 * time.Millisecond)

	if got := fetches.Load(); got != 0 {
		t.Errorf("metadata-only calls fetched data (%d FETCH_DATA) — an open must not hydrate a file just to read/set its metadata", got)
	}
	attrs, _, err := findAttrTag(file)
	if err != nil {
		t.Fatalf("findAttrTag: %v", err)
	}
	if attrs&fileAttrRecallOnDataAccess == 0 {
		t.Error("file no longer reads as online-only (RECALL_ON_DATA_ACCESS cleared) after metadata-only calls")
	}

	// RefreshPlaceholder — the FsMetadata + dehydrate path that runs on every
	// server-side change of a downloaded file — has the most to lose if a
	// Windows build ever enforced the WRITE_DATA access Microsoft's
	// CfUpdatePlaceholder page documents: every refresh would fail, reconcile
	// would never record the directory's ETag, and a downloaded file would go
	// stale for good. Refresh the still-online-only meta.bin to a new
	// size/mtime on the attributes-only handle: it must succeed, fetch
	// nothing, and leave the file online-only and in-sync.
	refreshed := time.Now().Add(time.Hour).Truncate(time.Second)
	if err := RefreshPlaceholder(file, identity, 8192, refreshed); err != nil {
		t.Fatalf("RefreshPlaceholder on an attributes-only handle: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if got := fetches.Load(); got != 0 {
		t.Errorf("RefreshPlaceholder fetched data (%d FETCH_DATA) — a refresh must never pull the file down", got)
	}
	if fi, serr := os.Stat(file); serr != nil {
		t.Fatalf("stat after the refresh: %v", serr)
	} else if fi.Size() != 8192 {
		t.Errorf("size after the refresh = %d, want 8192 (the new server size)", fi.Size())
	}
	if attrs, _, aerr := findAttrTag(file); aerr != nil {
		t.Fatalf("findAttrTag after the refresh: %v", aerr)
	} else if attrs&fileAttrRecallOnDataAccess == 0 {
		t.Error("the refreshed file is no longer online-only — the refresh hydrated it")
	}
	if ch, cerr := Inspect(file); cerr != nil {
		t.Fatalf("Inspect after the refresh: %v", cerr)
	} else if ch.NeedsUpload {
		t.Error("the refreshed placeholder is not in sync — MARK_IN_SYNC did not take")
	}

	// UpdateIdentityKeepState (CF_UPDATE_FLAG_NONE) repoints a file whose edit
	// is still waiting to upload, so the one thing it must not do is stamp it
	// in-sync — that bit is what stops a later refresh dehydrating the only
	// copy of the edit away. Nothing before this exercised flags 0 live.
	if err := CreatePlaceholders(root, []PlaceholderInfo{
		{Name: "keep.bin", Size: 4096, ModTime: time.Now(), Identity: []byte("remote/keep.bin")},
	}); err != nil {
		t.Fatalf("CreatePlaceholders(keep.bin): %v", err)
	}
	keep := filepath.Join(root, "keep.bin")
	// A genuine pending edit: hydrate it (this one DOES fetch, deliberately —
	// every fetch assertion above is done), then write to it.
	if _, rerr := os.ReadFile(keep); rerr != nil {
		t.Fatalf("read keep.bin: %v", rerr)
	}
	kf, oerr := os.OpenFile(keep, os.O_WRONLY, 0o644)
	if oerr != nil {
		t.Fatalf("open keep.bin for writing: %v", oerr)
	}
	if _, werr := kf.WriteAt([]byte("PENDING EDIT"), 0); werr != nil {
		kf.Close()
		t.Fatalf("write keep.bin: %v", werr)
	}
	if cerr := kf.Close(); cerr != nil {
		t.Fatalf("close keep.bin: %v", cerr)
	}
	time.Sleep(300 * time.Millisecond)
	if mod, merr := PlaceholderModified(keep); merr != nil {
		t.Fatalf("PlaceholderModified(keep.bin): %v", merr)
	} else if !mod {
		t.Fatal("keep.bin does not read as edited — the rest of this leg would prove nothing")
	}
	moved := []byte("remote/sub/keep.bin")
	if err := UpdateIdentityKeepState(keep, moved); err != nil {
		t.Fatalf("UpdateIdentityKeepState (CF_UPDATE_FLAG_NONE) on an attributes-only handle: %v", err)
	}
	if id, ierr := PlaceholderIdentity(keep); ierr != nil {
		t.Fatalf("PlaceholderIdentity after the keep-state repoint: %v", ierr)
	} else if string(id) != string(moved) {
		t.Errorf("identity after the keep-state repoint = %q, want %q", id, moved)
	}
	if mod, merr := PlaceholderModified(keep); merr != nil {
		t.Fatalf("PlaceholderModified after the keep-state repoint: %v", merr)
	} else if !mod {
		t.Error("the keep-state repoint dropped the pending edit's modified data")
	}
	if ch, cerr := Inspect(keep); cerr != nil {
		t.Fatalf("Inspect after the keep-state repoint: %v", cerr)
	} else if !ch.NeedsUpload {
		t.Error("the keep-state repoint stamped the file in-sync — the pending edit is now invisible to write-back")
	}

	// RevertPlaceholder is the one openForCloud caller the checks above don't
	// exercise, and Microsoft's own docs disagree with themselves about it
	// (the CfRevertPlaceholder page says an attribute handle is enough, a
	// remark elsewhere mentions WRITE_DATA). It runs on hydrated placeholders
	// only — leaving virtual-files mode, and salvaging an unshared folder —
	// and it must both succeed on an attributes-only handle and leave the
	// file's bytes in place as a plain file.
	if err := CreatePlaceholders(root, []PlaceholderInfo{
		{Name: "revert.bin", Size: 4096, ModTime: time.Now(), Identity: []byte("remote/revert.bin")},
	}); err != nil {
		t.Fatalf("CreatePlaceholders(revert.bin): %v", err)
	}
	rev := filepath.Join(root, "revert.bin")
	before, err := os.ReadFile(rev) // hydrates it: reverting needs the bytes present
	if err != nil {
		t.Fatalf("read revert.bin: %v", err)
	}
	if len(before) != 4096 {
		t.Fatalf("read %d bytes before the revert, want 4096", len(before))
	}
	if err := RevertPlaceholder(rev); err != nil {
		t.Fatalf("RevertPlaceholder on an attributes-only handle: %v", err)
	}
	if ph, err := IsPlaceholder(rev); err != nil {
		t.Errorf("IsPlaceholder after the revert: %v", err)
	} else if ph {
		t.Error("file is still a placeholder after RevertPlaceholder")
	}
	after, err := os.ReadFile(rev)
	if err != nil {
		t.Fatalf("read revert.bin after the revert: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("read %d bytes after the revert, want %d — the data did not survive", len(after), len(before))
	}
}
