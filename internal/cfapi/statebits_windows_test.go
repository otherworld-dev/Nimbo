//go:build windows

package cfapi

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// TestStateBitsAcrossLifecycle measures, on the live driver, how the
// CF_PLACEHOLDER_STATE bits move through a file's and a directory's life. Two
// production decisions hang on the answers (Deck #576):
//
//  1. Does HYDRATION clear a file's in-sync bit? If yes, every opened file
//     shows the "sync pending" arrows forever and the hydrate path must
//     re-mark; if no, the file arrows seen in the field have another cause.
//  2. Can the sweep heal the directories older versions left not in sync,
//     populated AND never-opened ones? Marking a never-opened one in sync
//     must leave it able to populate on its first open
//     (TestInSyncDirStillPopulates; the belief that it could not came from
//     listing from the provider's own process, which never populates).
//
// Opt in with NIMBO_CFAPI_LIVE=1.
func TestStateBitsAcrossLifecycle(t *testing.T) {
	if os.Getenv("NIMBO_CFAPI_LIVE") == "" {
		t.Skip("set NIMBO_CFAPI_LIVE=1 to run against the real Cloud Files API")
	}
	if !Supported() {
		t.Skip("Cloud Files API not available")
	}
	root := filepath.Join(t.TempDir(), "stateroot")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Purge(root) })

	oldDebug := Debug
	Debug = func(format string, args ...any) { t.Logf("dbg: "+format, args...) }
	t.Cleanup(func() { Debug = oldDebug })

	var listCalls atomic.Int32
	list := func(rel string) []PlaceholderInfo {
		listCalls.Add(1)
		if rel == "lazydir" || rel == "untouched" {
			return []PlaceholderInfo{{Name: "kid.txt", Size: 3, ModTime: time.Now(), Identity: []byte("remote/" + rel + "/kid.txt")}}
		}
		// The root must return at least one entry, mirroring production, where
		// Mount seeds the root's top level eagerly. A root whose transfer
		// delivers ZERO entries reproduces an infinite FETCH_PLACEHOLDERS storm
		// for "" (same transferKey re-issued forever, count=0 completions
		// accepted but ignored) — measured here 2026-08-18, recorded on Deck
		// #569 as a likely mechanism for the field spin loop.
		if rel == "" {
			return []PlaceholderInfo{{Name: "seed.txt", Size: 1, ModTime: time.Now(), Identity: []byte("remote/seed.txt")}}
		}
		return []PlaceholderInfo{}
	}
	hydrate := func(identity []byte, offset, length int64) ([]byte, error) {
		return make([]byte, length), nil
	}
	connKey, err := Mount(root, "NimboStateTest", "", hydrate, list)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	defer Unmount(root, connKey)

	if err := CreatePlaceholders(root, []PlaceholderInfo{
		{Name: "doc.bin", Size: 16, ModTime: time.Now(), Identity: []byte("remote/doc.bin")},
		{Name: "lazydir", IsDir: true, ModTime: time.Now(), Identity: []byte("remote/lazydir")},
		{Name: "untouched", IsDir: true, ModTime: time.Now(), Identity: []byte("remote/untouched")},
	}); err != nil {
		t.Fatalf("CreatePlaceholders: %v", err)
	}
	file := filepath.Join(root, "doc.bin")
	dir := filepath.Join(root, "lazydir")
	untouched := filepath.Join(root, "untouched")

	// --- Question 1: file across hydration ---
	before := placeholderStateOf(t, file)
	t.Logf("file before hydration: state=0x%02x in-sync=%v", before, before&cfPlaceholderStateInSync != 0)
	if before&cfPlaceholderStateInSync == 0 {
		t.Error("fresh file placeholder is not in-sync despite the MARK_IN_SYNC create flag")
	}

	data, err := os.ReadFile(file) // triggers FETCH_DATA hydration
	if err != nil {
		t.Fatalf("hydrating read: %v", err)
	}
	if len(data) != 16 {
		t.Fatalf("hydrated %d bytes, want 16", len(data))
	}
	after := placeholderStateOf(t, file)
	t.Logf("file after hydration:  state=0x%02x in-sync=%v", after, after&cfPlaceholderStateInSync != 0)
	if after&cfPlaceholderStateInSync == 0 {
		t.Log("ANSWER 1: hydration CLEARS the in-sync bit -> the hydrate path must re-mark after transfer")
	} else {
		t.Log("ANSWER 1: hydration preserves the in-sync bit -> file arrows in the field have another cause")
	}

	// --- Question 2: directory states and the sweep ---
	dBefore := placeholderStateOf(t, dir)
	t.Logf("lazy dir signature: state=0x%02x (PARTIAL + IN_SYNC expected)", dBefore)
	if dBefore&cfPlaceholderStatePartial == 0 {
		t.Errorf("lazy dir lacks the PARTIAL bit (state=0x%02x)", dBefore)
	}
	if dBefore&cfPlaceholderStateInSync == 0 {
		t.Errorf("lazy dir was not created in sync (state=0x%02x) - Explorer draws the arrows on it", dBefore)
	}

	// The two states older versions left directories in, both not in sync:
	// populated (opened once) and never opened.
	if err := CreatePlaceholders(root, []PlaceholderInfo{
		{Name: "popdir", IsDir: true, ModTime: time.Now(), Identity: []byte("remote/popdir")},
	}); err != nil {
		t.Fatalf("CreatePlaceholders(popdir): %v", err)
	}
	popdir := filepath.Join(root, "popdir")
	if err := setPopulatedNotInSync(popdir); err != nil {
		t.Fatalf("setPopulatedNotInSync: %v", err)
	}
	clearInSync(t, popdir)
	clearInSync(t, untouched)
	if s := placeholderStateOf(t, popdir); s&(cfPlaceholderStatePartial|cfPlaceholderStateInSync) != 0 {
		t.Fatalf("setup failed: popdir state=0x%02x, want populated and not in sync", s)
	}

	// --- The sweep marks both ---
	marked := SweepDirsInSync(root, 0)
	t.Logf("SweepDirsInSync marked %d directorie(s)", marked)
	for _, d := range []string{popdir, untouched} {
		if s := placeholderStateOf(t, d); s&cfPlaceholderStateInSync == 0 {
			t.Errorf("%s not marked in sync by the sweep (state=0x%02x)", filepath.Base(d), s)
		}
	}
	if _, err := os.ReadDir(popdir); err != nil {
		t.Fatalf("ReadDir after mark: %v", err)
	}
	// The sweep must not touch population: the never-opened folder is still
	// lazy, and still fills in on its first open from another process.
	if s := placeholderStateOf(t, untouched); s&cfPlaceholderStatePartial == 0 {
		t.Errorf("unpopulated dir lost its PARTIAL bit after the sweep (state=0x%02x)", s)
	}
	if out, err := exec.Command("cmd", "/c", "dir", "/b", untouched).CombinedOutput(); err != nil {
		t.Fatalf("dir: %v: %s", err, out)
	}
	if names := listNames(t, untouched); len(names) != 1 || names[0] != "kid.txt" {
		t.Errorf("the swept never-opened folder lists %v after its first open, want [kid.txt]", names)
	}
}

// setPopulatedNotInSync clears a directory placeholder's on-demand population
// (marking it complete) WITHOUT giving it the in-sync state, reproducing the
// exact on-disk state of folders populated before the in-sync marking existed.
func setPopulatedNotInSync(dir string) error {
	pathW, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(pathW,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	var usn int64
	// CF_UPDATE_FLAG_DISABLE_ON_DEMAND_POPULATION = 0x10 (cfapi.h 10.0.26100).
	hr, _, _ := procCfUpdatePlaceholder.Call(uintptr(h), 0, 0, 0, 0, 0, uintptr(0x10), uintptr(unsafe.Pointer(&usn)), 0)
	if int32(hr) < 0 {
		return fmt.Errorf("CfUpdatePlaceholder(disable population): 0x%08x", uint32(hr))
	}
	return nil
}
