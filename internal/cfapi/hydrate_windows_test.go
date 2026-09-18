//go:build windows

package cfapi

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestHydrateIfPinnedLive pins the provider's half of the pin contract: a
// pinned, online-only placeholder is downloaded by HydrateIfPinned; an
// unpinned one is left alone; a second call is a no-op. Field evidence
// (issue #7): CfSetPinState(PINNED) alone downloads nothing — the customer's
// pinned folders sat on "sync pending" for a day.
// Live-driver test, opt in with NIMBO_CFAPI_LIVE=1.
func TestHydrateIfPinnedLive(t *testing.T) {
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

	var fetches atomic.Int64
	hydrate := func(identity []byte, offset, length int64) ([]byte, error) {
		fetches.Add(1)
		return make([]byte, length), nil
	}
	list := func(rel string) []PlaceholderInfo { return []PlaceholderInfo{} }
	connKey, err := Mount(root, "NimboPinTest", "", hydrate, list)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	defer Unmount(root, connKey)

	if err := CreatePlaceholders(root, []PlaceholderInfo{
		{Name: "pinned.bin", Size: 4096, ModTime: time.Now(), Identity: []byte("remote/pinned.bin")},
		{Name: "loose.bin", Size: 4096, ModTime: time.Now(), Identity: []byte("remote/loose.bin")},
	}); err != nil {
		t.Fatalf("CreatePlaceholders: %v", err)
	}
	pinned := filepath.Join(root, "pinned.bin")
	loose := filepath.Join(root, "loose.bin")

	if PinnedDehydrated(pinned) {
		t.Fatal("fresh placeholder reads as pinned before SetPinState")
	}
	if err := SetPinState(pinned, true, false); err != nil {
		t.Fatalf("SetPinState: %v", err)
	}
	if !PinnedDehydrated(pinned) {
		t.Fatal("pinned online-only placeholder not recognised")
	}

	ok, err := HydrateIfPinned(pinned)
	if err != nil {
		t.Fatalf("HydrateIfPinned: %v", err)
	}
	if !ok {
		t.Fatal("HydrateIfPinned did nothing for a pinned online-only file")
	}
	if fetches.Load() == 0 {
		t.Fatal("no FETCH_DATA reached the provider — nothing was downloaded")
	}
	if PinnedDehydrated(pinned) {
		t.Error("file still reads as online-only after hydration")
	}

	base := fetches.Load()
	if ok, err := HydrateIfPinned(pinned); err != nil || ok {
		t.Errorf("second HydrateIfPinned = (%v, %v), want (false, nil)", ok, err)
	}
	if ok, err := HydrateIfPinned(loose); err != nil || ok {
		t.Errorf("unpinned file: HydrateIfPinned = (%v, %v), want (false, nil)", ok, err)
	}
	if got := fetches.Load(); got != base {
		t.Errorf("no-op calls fetched data (%d extra FETCH_DATA)", got-base)
	}
}
