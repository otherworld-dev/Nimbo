//go:build windows

package cfapi

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// TestInSyncDirStillPopulates pins the driver behaviour directory placeholders
// are built on: a lazy directory that is IN SYNC before anything has opened it
// still asks the provider to populate it on its first open, gets its
// children, and stays in sync afterwards. So folders are created in sync
// (buildPlaceholders), and a folder nobody has opened shows Explorer's cloud
// rather than the "sync pending" arrows (GitHub #17).
//
// This test once asserted the opposite (Deck #576, 2026-08-18) and every
// folder was created not in sync because of it, arrows and all. Its mistake
// was listing the folder with os.ReadDir from the test process, which is the
// provider, and the filter never populates for the provider's own listings
// whatever the folder's state. Listing from a child process, as Explorer
// lists from its own, is what drives population. Both kinds of lazy folder
// are covered: one created by CreatePlaceholders (reconcile's pull) and one
// created inside a FETCH_PLACEHOLDERS transfer (a subfolder of an opened
// folder).
//
// Live-driver test, opt in with NIMBO_CFAPI_LIVE=1.
func TestInSyncDirStillPopulates(t *testing.T) {
	if os.Getenv("NIMBO_CFAPI_LIVE") == "" {
		t.Skip("set NIMBO_CFAPI_LIVE=1 to run against the real Cloud Files API")
	}
	if !Supported() {
		t.Skip("Cloud Files API not available")
	}
	root := filepath.Join(t.TempDir(), "insyncroot")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Purge(root) })

	var mu sync.Mutex
	fetches := map[string]int{}
	list := func(rel string) []PlaceholderInfo {
		mu.Lock()
		fetches[rel]++
		mu.Unlock()
		// The root must return at least one entry, mirroring production, where
		// Mount seeds the root's top level eagerly. A root whose transfer
		// delivers ZERO entries reproduces an infinite FETCH_PLACEHOLDERS storm
		// for "" (measured 2026-08-18, Deck #569).
		if rel == "" {
			return []PlaceholderInfo{{Name: "seed.txt", Size: 1, ModTime: time.Now(), Identity: []byte("remote/seed.txt")}}
		}
		return []PlaceholderInfo{
			{Name: "child.txt", Size: 4, ModTime: time.Now(), Identity: []byte("remote/" + rel + "/child.txt")},
			{Name: "inner", IsDir: true, ModTime: time.Now(), Identity: []byte("remote/" + rel + "/inner")},
		}
	}
	hydrate := func(identity []byte, offset, length int64) ([]byte, error) {
		return make([]byte, length), nil
	}
	connKey, err := Mount(root, "NimboInSyncTest", "", hydrate, list)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	defer Unmount(root, connKey)

	if err := CreatePlaceholders(root, []PlaceholderInfo{
		{Name: "sub", IsDir: true, ModTime: time.Now(), Identity: []byte("remote/sub")},
	}); err != nil {
		t.Fatalf("CreatePlaceholders: %v", err)
	}

	// "sub" comes from CreatePlaceholders; "sub/inner" is created by the
	// transfer that populates "sub".
	for _, rel := range []string{"sub", "sub/inner"} {
		dir := filepath.Join(root, filepath.FromSlash(rel))
		attrs := placeholderAttrs(t, dir)
		if s := placeholderStateOf(t, dir); s&cfPlaceholderStateInSync == 0 || attrs&fileAttrRecallOnDataAccess == 0 {
			t.Fatalf("%s: want a lazy directory created in sync, got attrs=0x%x state=0x%x", rel, attrs, s)
		}
		out, err := exec.Command("cmd", "/c", "dir", "/b", dir).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: dir: %v: %s", rel, err, out)
		}
		mu.Lock()
		n := fetches[rel]
		mu.Unlock()
		if n == 0 {
			t.Errorf("%s: the in-sync lazy directory never asked to be populated", rel)
		}
		got := map[string]bool{}
		for _, e := range listNames(t, dir) {
			got[e] = true
		}
		if !got["child.txt"] || !got["inner"] {
			t.Errorf("%s: after the open it holds %v, want child.txt and inner", rel, got)
		}
		if s := placeholderStateOf(t, dir); s&cfPlaceholderStateInSync == 0 {
			t.Errorf("%s: population cleared the in-sync bit (state=0x%x) - Explorer would show the arrows", rel, s)
		}
	}
}

func placeholderAttrs(t *testing.T, path string) uint32 {
	t.Helper()
	attrs, _, err := findAttrTag(path)
	if err != nil {
		t.Fatalf("findAttrTag(%s): %v", path, err)
	}
	return attrs
}

func listNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// clearInSync puts a placeholder back to NOT in sync, the state older versions
// created every directory in, for tests of what heals them.
func clearInSync(t *testing.T, path string) {
	t.Helper()
	h, err := openForCloud(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer windows.CloseHandle(h)
	var usn int64
	hr, _, _ := procCfSetInSyncState.Call(uintptr(h), 0 /* CF_IN_SYNC_STATE_NOT_IN_SYNC */, uintptr(cfSetInSyncFlagNone), uintptr(unsafe.Pointer(&usn)))
	if int32(hr) < 0 {
		t.Fatalf("CfSetInSyncState(NOT_IN_SYNC) %s: 0x%08x", path, uint32(hr))
	}
}

func placeholderStateOf(t *testing.T, path string) uint32 {
	t.Helper()
	attrs, tag, err := findAttrTag(path)
	if err != nil {
		t.Fatalf("findAttrTag(%s): %v", path, err)
	}
	r1, _, _ := procCfGetPlaceholderStateFromAttrTag.Call(uintptr(attrs), uintptr(tag))
	state := uint32(r1)
	if state == cfPlaceholderStateInvalid {
		return 0
	}
	return state
}
