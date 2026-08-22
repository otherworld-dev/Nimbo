//go:build windows

package cfapi

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestDehydrateAlreadyDehydratedDoesNotFetch pins the free-up-space re-download
// (VM, 2026-08-19): CfDehydratePlaceholder on an ALREADY-dehydrated placeholder
// makes the filter fetch the file's full content first (FETCH_DATA offset=0,
// full length — named in the forensic log) only to discard it again. Nimbo's
// folder-free walks every file blindly, so freeing a mostly-freed folder
// re-downloaded everything. Dehydrate must no-op on files that already have no
// data.
//
// Live-driver test, opt in with NIMBO_CFAPI_LIVE=1.
func TestDehydrateAlreadyDehydratedDoesNotFetch(t *testing.T) {
	if os.Getenv("NIMBO_CFAPI_LIVE") == "" {
		t.Skip("set NIMBO_CFAPI_LIVE=1 to run against the real Cloud Files API")
	}
	if !Supported() {
		t.Skip("Cloud Files API not available")
	}
	root := filepath.Join(t.TempDir(), "dehyroot")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Purge(root) })

	var fetches atomic.Int64
	list := func(rel string) []PlaceholderInfo { return nil }
	hydrate := func(identity []byte, offset, length int64) ([]byte, error) {
		fetches.Add(1)
		return make([]byte, length), nil
	}
	connKey, err := Mount(root, "NimboDehyTest", "", hydrate, list)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	defer Unmount(root, connKey)

	if err := CreatePlaceholders(root, []PlaceholderInfo{
		{Name: "f.bin", Size: 4096, ModTime: time.Now(), Identity: []byte("remote/f.bin")},
	}); err != nil {
		t.Fatalf("CreatePlaceholders: %v", err)
	}
	file := filepath.Join(root, "f.bin")
	if _, err := os.ReadFile(file); err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	if err := Dehydrate(file); err != nil {
		t.Fatalf("first dehydrate: %v", err)
	}
	base := fetches.Load()

	// The bug: this second call fetched the whole file before discarding it.
	if err := Dehydrate(file); err != nil {
		t.Fatalf("second dehydrate: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	if got := fetches.Load(); got != base {
		t.Errorf("dehydrating an already-dehydrated file fetched data (%d extra FETCH_DATA) — a full download thrown straight away", got-base)
	}
}
