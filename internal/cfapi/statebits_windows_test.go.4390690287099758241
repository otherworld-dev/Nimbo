//go:build windows

package cfapi

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// pollReadDir re-enumerates dir until it holds at least want entries or a
// deadline passes, returning whatever it last saw. Population delivery is
// asynchronous relative to the enumeration that triggers it.
func pollReadDir(t *testing.T, dir string, want int) []os.DirEntry {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("ReadDir(%s): %v", dir, err)
		}
		if len(entries) >= want || time.Now().After(deadline) {
			return entries
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestStateBitsAcrossLifecycle measures, on the live driver, how the
// CF_PLACEHOLDER_STATE bits move through a file's and a directory's life. Two
// production decisions hang on the answers (Deck #576):
//
//  1. Does HYDRATION clear a file's in-sync bit? If yes, every opened file
//     shows the "sync pending" arrows forever and the hydrate path must
//     re-mark; if no, the file arrows seen in the field have another cause.
//  2. What distinguishes a POPULATED-but-unmarked directory from an
//     unpopulated one? Old mounts predate the after-population mark, so a
//     heal pass needs an on-disk signature to find them safely — marking an
//     UNpopulated dir in-sync makes it enumerate empty forever.
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
		if rel == "lazydir" {
			return []PlaceholderInfo{{Name: "kid.txt", Size: 3, ModTime: time.Now(), Identity: []byte("remote/lazydir/kid.txt")}}
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
	t.Logf("lazy dir signature: state=0x%02x (PARTIAL expected)", dBefore)
	if dBefore&cfPlaceholderStatePartial == 0 {
		t.Errorf("lazy dir lacks the PARTIAL bit (state=0x%02x) — the sweep's unpopulated-signature is wrong", dBefore)
	}

	// OPEN QUESTION, logged not failed: enumerating this partial dir does not
	// fire FETCH_PLACEHOLDERS for it in this harness (the parent root's fetch
	// fires instead, and nothing follows for the subdir). Production populates
	// subdirs, so something environmental differs; tracked on Deck #569.
	entries := pollReadDir(t, dir, 1)
	t.Logf("open question: lazy-dir enumeration returned %d entries (FETCH_PLACEHOLDERS calls=%d)", len(entries), listCalls.Load())

	// For the sweep itself, build the populated-but-unmarked state DIRECTLY:
	// a dir created population-complete (DISABLE_ON_DEMAND_POPULATION) but NOT
	// in-sync — exactly what an old mount's opened folders look like.
	if err := CreatePlaceholders(root, []PlaceholderInfo{
		{Name: "popdir", IsDir: true, ModTime: time.Now(), Identity: []byte("remote/popdir")},
	}); err != nil {
		t.Fatalf("CreatePlaceholders(popdir): %v", err)
	}
	popdir := filepath.Join(root, "popdir")
	if err := setPopulatedNotInSync(popdir); err != nil {
		t.Fatalf("setPopulatedNotInSync: %v", err)
	}
	dAfter := placeholderStateOf(t, popdir)
	t.Logf("populated-but-unmarked dir: state=0x%02x", dAfter)
	if dAfter&cfPlaceholderStatePartial != 0 {
		t.Fatalf("setup failed: popdir still PARTIAL (state=0x%02x)", dAfter)
	}
	dir = popdir // the sweep assertions below target the populated dir

	// --- The sweep: marks the populated dir, leaves the unpopulated one alone ---
	marked := SweepDirsInSync(root, 0)
	t.Logf("SweepDirsInSync marked %d directorie(s)", marked)
	if s := placeholderStateOf(t, dir); s&cfPlaceholderStateInSync == 0 {
		t.Errorf("populated dir not marked in-sync by the sweep (state=0x%02x)", s)
	}
	if s := placeholderStateOf(t, untouched); s&cfPlaceholderStateInSync != 0 {
		t.Errorf("UNPOPULATED dir was marked in-sync by the sweep (state=0x%02x) — it would enumerate empty forever", s)
	}

	// After the mark, the populated dir must still enumerate (it is empty and
	// population-complete, so empty is the correct answer — and it must not
	// hang or refire fetches).
	if _, err := os.ReadDir(dir); err != nil {
		t.Fatalf("ReadDir after mark: %v", err)
	}
	// The unpopulated dir must still carry its PARTIAL bit after the sweep —
	// i.e. the sweep must not have touched its population state. (Whether
	// enumeration fires its fetch is the open question above, so the on-disk
	// bit is the assertion here.)
	if s := placeholderStateOf(t, untouched); s&cfPlaceholderStatePartial == 0 {
		t.Errorf("unpopulated dir lost its PARTIAL bit after the sweep (state=0x%02x)", s)
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
