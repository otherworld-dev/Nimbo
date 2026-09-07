//go:build windows

package cfapi

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestInSyncDirStillPopulates settles the claim behind Deck #576.
//
// buildPlaceholders creates directory placeholders NOT in-sync, and the comment
// asserts that is what makes the shell issue FETCH_PLACEHOLDERS on first open.
// The cost of that design is Explorer's Status column showing the "sync
// pending" arrows on every folder forever.
//
// This test asks the driver directly whether the claim is true: create a lazy
// directory placeholder, force it IN-SYNC before anything enumerates it, then
// enumerate and see whether the filter still asks the provider to populate it.
// If the children arrive, in-sync state and population state are independent
// axes, the not-in-sync design buys nothing, and folders can carry the correct
// status.
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

	var listCalls atomic.Int32
	list := func(rel string) []PlaceholderInfo {
		listCalls.Add(1)
		t.Logf("FETCH_PLACEHOLDERS for %q", rel)
		if rel == "sub" {
			return []PlaceholderInfo{
				{Name: "child.txt", Size: 4, ModTime: time.Now(), Identity: []byte("remote/sub/child.txt")},
			}
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
	sub := filepath.Join(root, "sub")

	// Force the directory in-sync BEFORE anything enumerates it.
	if err := MarkInSync(sub, []byte("remote/sub")); err != nil {
		t.Fatalf("MarkInSync(dir): %v", err)
	}
	if s := placeholderStateOf(t, sub); s&cfPlaceholderStateInSync == 0 {
		t.Fatalf("setup failed: dir state=0x%x, in-sync bit not set", s)
	}

	// Now enumerate it. If population still works, the child appears. Delivery
	// is asynchronous, so give a genuinely-working population ample time to
	// land before concluding it never fires.
	if _, err := os.ReadDir(sub); err != nil {
		t.Fatalf("ReadDir(sub): %v", err)
	}
	var names []string
	deadline := time.Now().Add(3 * time.Second)
	for {
		entries, err := os.ReadDir(sub)
		if err != nil {
			t.Fatalf("ReadDir(sub): %v", err)
		}
		names = names[:0]
		for _, e := range entries {
			names = append(names, e.Name())
		}
		if len(names) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("enumeration returned %v after %d FETCH_PLACEHOLDERS call(s)", names, listCalls.Load())

	// MEASURED 2026-08-18 on the live driver: the in-sync directory does NOT
	// populate — no FETCH_PLACEHOLDERS fires for it and enumeration comes back
	// empty. Directory in-sync state and population are coupled by the filter,
	// so the not-in-sync creation design is LOAD-BEARING: dirs must be created
	// not-in-sync, and may only be marked in-sync AFTER their population has
	// actually happened (which is when "populated and matches the server" is
	// true anyway). This assertion pins that driver behaviour; if a future
	// Windows decouples the two, this failing is worth knowing about.
	if len(names) != 0 {
		t.Errorf("driver behaviour changed: an in-sync directory now populates (got %v) — dirs could be created in-sync again", names)
	}
	if got := listCalls.Load(); got > 2 { // mount seed + at most one root fetch
		t.Errorf("driver behaviour changed: %d FETCH_PLACEHOLDERS calls, want <=2 (none may be for the in-sync dir)", got)
	}

	// And does the filter leave the in-sync bit alone across population?
	if s := placeholderStateOf(t, sub); s&cfPlaceholderStateInSync == 0 {
		t.Logf("note: population CLEARED the in-sync bit (state=0x%x) — a post-population re-mark would be needed", s)
	} else {
		t.Log("in-sync bit survived population")
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
