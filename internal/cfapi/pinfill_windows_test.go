//go:build windows

package cfapi

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// placeholderState reads a path's CF_PLACEHOLDER_STATE bits, attributes only.
func placeholderState(t *testing.T, path string) (attrs, state uint32) {
	t.Helper()
	a, tag, err := findAttrTag(path)
	if err != nil {
		t.Fatalf("findAttrTag(%s): %v", path, err)
	}
	r1, _, _ := procCfGetPlaceholderStateFromAttrTag.Call(uintptr(a), uintptr(tag))
	return a, uint32(r1)
}

// TestMarkDirPopulatedLive pins what "keep on this device" for a never-opened
// folder (GitHub #17) stands on: the provider can fill a lazy directory
// itself. CfCreatePlaceholders into it works, the children inherit the
// folder's pin, MarkDirPopulated then clears "not fetched yet" and sets
// in-sync from outside any callback, and those bits survive a child being
// created or downloaded afterwards.
// Live-driver test, opt in with NIMBO_CFAPI_LIVE=1.
func TestMarkDirPopulatedLive(t *testing.T) {
	if os.Getenv("NIMBO_CFAPI_LIVE") == "" {
		t.Skip("set NIMBO_CFAPI_LIVE=1 to run against the real Cloud Files API")
	}
	if !Supported() {
		t.Skip("Cloud Files API not available")
	}
	root := filepath.Join(t.TempDir(), "pinfillroot")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Purge(root) })
	list := func(rel string) []PlaceholderInfo { return []PlaceholderInfo{} }
	hydrate := func(identity []byte, offset, length int64) ([]byte, error) { return make([]byte, length), nil }
	connKey, err := Mount(root, "NimboPinFillTest", "", hydrate, list)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	defer Unmount(root, connKey)

	if err := CreatePlaceholders(root, []PlaceholderInfo{
		{Name: "kept", IsDir: true, ModTime: time.Now(), Identity: []byte("remote/kept")},
	}); err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(root, "kept")
	if err := SetPinState(kept, true, true); err != nil {
		t.Fatalf("SetPinState: %v", err)
	}
	if !Pinned(kept) {
		t.Fatal("a pinned lazy folder does not read as pinned")
	}
	if pop, _ := DirPopulated(kept); pop {
		t.Fatal("premise: the folder should still be lazy")
	}

	if err := CreatePlaceholders(kept, []PlaceholderInfo{
		{Name: "child.bin", Size: 10, ModTime: time.Now(), Identity: []byte("remote/kept/child.bin")},
		{Name: "sub", IsDir: true, ModTime: time.Now(), Identity: []byte("remote/kept/sub")},
	}); err != nil {
		t.Fatalf("CreatePlaceholders into a lazy folder: %v", err)
	}
	child := filepath.Join(kept, "child.bin")
	if !PinnedDehydrated(child) {
		t.Error("a file created in a pinned folder did not inherit the pin")
	}
	if !Pinned(filepath.Join(kept, "sub")) {
		t.Error("a folder created in a pinned folder did not inherit the pin")
	}

	if err := MarkDirPopulated(kept); err != nil {
		t.Fatalf("MarkDirPopulated: %v", err)
	}
	check := func(when string) {
		t.Helper()
		attrs, state := placeholderState(t, kept)
		if attrs&fileAttrRecallOnDataAccess != 0 || state&cfPlaceholderStatePartial != 0 {
			t.Errorf("%s: still reads as not fetched (attrs=0x%x state=0x%x)", when, attrs, state)
		}
		if state&cfPlaceholderStateInSync == 0 {
			t.Errorf("%s: not in sync (state=0x%x) - Explorer keeps the pending arrows", when, state)
		}
	}
	check("after MarkDirPopulated")

	if err := CreatePlaceholders(kept, []PlaceholderInfo{
		{Name: "late.bin", Size: 10, ModTime: time.Now(), Identity: []byte("remote/kept/late.bin")},
	}); err != nil {
		t.Fatalf("CreatePlaceholders after the mark: %v", err)
	}
	check("after a child was created")
	if ok, err := HydrateIfPinned(child); err != nil || !ok {
		t.Fatalf("HydrateIfPinned = %v, %v", ok, err)
	}
	check("after a child was downloaded")

	names := map[string]bool{}
	ents, _ := os.ReadDir(kept)
	for _, e := range ents {
		names[e.Name()] = true
	}
	if !names["child.bin"] || !names["late.bin"] || !names["sub"] {
		t.Errorf("folder lists %v, want child.bin, late.bin and sub", names)
	}
}

// TestShellPopulatedDirSettlesLive: a folder the shell populates (another
// process opens it) is marked in sync once dirSettleDelay has passed, instead
// of waiting hours for SweepDirsInSync (GitHub #17's lingering arrows).
// Live-driver test, opt in with NIMBO_CFAPI_LIVE=1.
func TestShellPopulatedDirSettlesLive(t *testing.T) {
	if os.Getenv("NIMBO_CFAPI_LIVE") == "" {
		t.Skip("set NIMBO_CFAPI_LIVE=1 to run against the real Cloud Files API")
	}
	if !Supported() {
		t.Skip("Cloud Files API not available")
	}
	old := dirSettleDelay
	dirSettleDelay = 300 * time.Millisecond
	t.Cleanup(func() { dirSettleDelay = old })

	root := filepath.Join(t.TempDir(), "settleroot")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Purge(root) })
	list := func(rel string) []PlaceholderInfo {
		if rel == "" {
			return []PlaceholderInfo{}
		}
		return []PlaceholderInfo{{Name: "f.bin", Size: 10, ModTime: time.Now(), Identity: []byte("remote/" + rel + "/f.bin")}}
	}
	hydrate := func(identity []byte, offset, length int64) ([]byte, error) { return make([]byte, length), nil }
	connKey, err := Mount(root, "NimboSettleTest", "", hydrate, list)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	defer Unmount(root, connKey)
	if err := CreatePlaceholders(root, []PlaceholderInfo{
		{Name: "opened", IsDir: true, ModTime: time.Now(), Identity: []byte("remote/opened")},
	}); err != nil {
		t.Fatal(err)
	}
	opened := filepath.Join(root, "opened")

	// This process is the provider, and the filter does not populate for its
	// own enumerations; a child process's does.
	out, err := exec.Command("cmd", "/c", "dir", "/b", opened).CombinedOutput()
	if err != nil {
		t.Fatalf("dir: %v: %s", err, out)
	}
	if _, state := placeholderState(t, opened); state&cfPlaceholderStateInSync != 0 {
		t.Fatal("marked in sync straight after the transfer - that poisons the enumeration that asked for it")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, state := placeholderState(t, opened)
		if state&cfPlaceholderStateInSync != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a shell-populated folder was never marked in sync (state=0x%x)", state)
		}
		time.Sleep(50 * time.Millisecond)
	}
	ents, _ := os.ReadDir(opened)
	if len(ents) != 1 || ents[0].Name() != "f.bin" {
		t.Errorf("after settling the folder lists %v, want [f.bin]", ents)
	}
}
