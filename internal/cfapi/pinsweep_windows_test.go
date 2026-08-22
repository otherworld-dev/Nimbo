//go:build windows

package cfapi

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TestSweepSettlePins pins the provider's half of the pin contract
// (2026-08-19): Explorer's "Free up space" merely sets FILE_ATTRIBUTE_UNPINNED
// and waits for the PROVIDER to dehydrate — and an unpinned item that still
// has its data reads as a pending operation, drawing sync-pending arrows on
// the item and every ancestor directory, forever, because Nimbo never reacted.
// (The first fix cancelled the request by clearing the pin — wrong: the user
// asked for space back. Settling means COMPLETING it: dehydrate.)
//
// SweepSettlePins dehydrates unpinned + hydrated + in-sync + quiet FILES; a
// dehydrated unpinned item is a completed free-up-space and is left alone.
//
// Live-driver test, opt in with NIMBO_CFAPI_LIVE=1.
func TestSweepSettlePins(t *testing.T) {
	if os.Getenv("NIMBO_CFAPI_LIVE") == "" {
		t.Skip("set NIMBO_CFAPI_LIVE=1 to run against the real Cloud Files API")
	}
	if !Supported() {
		t.Skip("Cloud Files API not available")
	}
	root := filepath.Join(t.TempDir(), "pinroot")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Purge(root) })

	list := func(rel string) []PlaceholderInfo { return nil }
	hydrate := func(identity []byte, offset, length int64) ([]byte, error) {
		return make([]byte, length), nil
	}
	connKey, err := Mount(root, "NimboPinTest", "", hydrate, list)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	defer Unmount(root, connKey)

	if err := CreatePlaceholders(root, []PlaceholderInfo{
		{Name: "stale.bin", Size: 8, ModTime: time.Now(), Identity: []byte("remote/stale.bin")},
		{Name: "freed.bin", Size: 8, ModTime: time.Now(), Identity: []byte("remote/freed.bin")},
	}); err != nil {
		t.Fatalf("CreatePlaceholders: %v", err)
	}
	stale := filepath.Join(root, "stale.bin")
	freed := filepath.Join(root, "freed.bin")

	// stale.bin: hydrated, then unpinned WITHOUT dehydrating — the half-done
	// state the unregister sweeps left behind.
	if _, err := os.ReadFile(stale); err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	if err := SetPinState(stale, false, false); err != nil {
		t.Fatalf("unpin stale: %v", err)
	}
	// freed.bin: a genuinely completed free-up-space — unpinned AND dehydrated.
	if _, err := os.ReadFile(freed); err != nil {
		t.Fatalf("hydrate freed: %v", err)
	}
	if err := SetPinState(freed, false, false); err != nil {
		t.Fatalf("unpin freed: %v", err)
	}
	if err := Dehydrate(freed); err != nil {
		t.Fatalf("dehydrate freed: %v", err)
	}

	// localonly.txt: a plain file with no cloud copy (the sync-excluded
	// journals in the field), unpinned by a recursive free-up-space. There is
	// nothing to free — the request is void and the attribute must be cleared,
	// NOT the file dehydrated/deleted.
	local := filepath.Join(root, "localonly.txt")
	if err := os.WriteFile(local, []byte("only copy"), 0o644); err != nil {
		t.Fatal(err)
	}
	la, _, _ := findAttrTag(local)
	localW, _ := windows.UTF16PtrFromString(local)
	if err := windows.SetFileAttributes(localW, la|fileAttrUnpinned); err != nil {
		t.Fatalf("stamp unpinned: %v", err)
	}

	n := SweepSettlePins(root, 0) // zero quiesce: everything is eligible now

	if n != 2 {
		t.Errorf("settled %d pins, want exactly 2 (stale.bin dehydrated + localonly.txt attr cleared)", n)
	}
	lattrs, _, err := findAttrTag(local)
	if err != nil {
		t.Fatalf("findAttrTag localonly: %v", err)
	}
	if lattrs&fileAttrUnpinned != 0 {
		t.Errorf("localonly.txt still UNPINNED (attrs=0x%x)", lattrs)
	}
	if b, rerr := os.ReadFile(local); rerr != nil || string(b) != "only copy" {
		t.Fatalf("localonly.txt content damaged (err=%v)", rerr)
	}
	attrs, _, err := findAttrTag(stale)
	if err != nil {
		t.Fatalf("findAttrTag stale: %v", err)
	}
	if attrs&0x400000 == 0 {
		t.Errorf("stale.bin not dehydrated (attrs=0x%x) — the free-up-space request was not honoured", attrs)
	}
	fattrs, _, err := findAttrTag(freed)
	if err != nil {
		t.Fatalf("findAttrTag freed: %v", err)
	}
	if fattrs&0x400000 == 0 || fattrs&fileAttrUnpinned == 0 {
		t.Errorf("freed.bin disturbed (attrs=0x%x) — completed free-up-space must be left alone", fattrs)
	}
}
