//go:build windows

package cfapi

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDisconnectPreservesPlaceholders pins the fix for the update-restart
// flattener: every graceful shutdown used to call full Unmount, whose
// CfUnregisterSyncRoot makes Windows strip the cloud state from the whole tree
// - so each app update reverted every placeholder to a plain file (seen on the
// VM after both the .223 and .224 updates; the heal kept re-converting the
// same folders). Disconnect keeps the registration, so state must survive a
// disconnect/reconnect cycle untouched.
//
// Live-driver test, opt in with NIMBO_CFAPI_LIVE=1.
func TestDisconnectPreservesPlaceholders(t *testing.T) {
	if os.Getenv("NIMBO_CFAPI_LIVE") == "" {
		t.Skip("set NIMBO_CFAPI_LIVE=1 to run against the real Cloud Files API")
	}
	if !Supported() {
		t.Skip("Cloud Files API not available")
	}
	root := filepath.Join(t.TempDir(), "discroot")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Purge(root) })

	list := func(rel string) []PlaceholderInfo {
		if rel == "" {
			return []PlaceholderInfo{{Name: "seed.txt", Size: 1, ModTime: time.Now(), Identity: []byte("remote/seed.txt")}}
		}
		return []PlaceholderInfo{}
	}
	hydrate := func(identity []byte, offset, length int64) ([]byte, error) {
		return make([]byte, length), nil
	}

	connKey, err := Mount(root, "NimboDiscTest", "", hydrate, list)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	if err := CreatePlaceholders(root, []PlaceholderInfo{
		{Name: "keep.bin", Size: 8, ModTime: time.Now(), Identity: []byte("remote/keep.bin")},
	}); err != nil {
		t.Fatalf("CreatePlaceholders: %v", err)
	}
	file := filepath.Join(root, "keep.bin")
	if s := placeholderStateOf(t, file); s&cfPlaceholderStateInSync == 0 {
		t.Fatalf("setup: keep.bin not an in-sync placeholder (state=0x%02x)", s)
	}
	// Hydrate while connected, mirroring the VM (its flattened files were all
	// hydrated ones).
	if _, err := os.ReadFile(file); err != nil {
		t.Fatalf("hydrate: %v", err)
	}

	// The app-shutdown path under test.
	Disconnect(root, connKey)
	time.Sleep(2 * time.Second)

	// #580 RESOLVED 2026-08-18: the "flatten across Disconnect" this test
	// originally measured was a PROBE ARTIFACT — after Disconnect this
	// (unmanifested) test process is no longer a sync engine, and cfapi
	// DISGUISES reparse points from it: hydrated placeholders read as plain
	// 0x20 while dehydrated keep RECALL_ON_DATA_ACCESS. fsutil is
	// %systemroot%-exempt and showed every reparse point intact all along, so
	// it is the probe of record here (see the disguising GOTCHA note in
	// cfapi_windows.go). The REAL field flattening was the documented
	// CfUnregisterSyncRoot sweep in the pre-890be88 shutdown path. This is now
	// a hard invariant: Disconnect must preserve cloud state.
	out, _ := exec.Command("fsutil", "reparsepoint", "query", file).CombinedOutput()
	if !strings.Contains(string(out), "Tag value") {
		t.Fatalf("hydrated placeholder flattened across Disconnect — fsutil: %s", strings.TrimSpace(string(out)))
	}

	// And the next session must reconnect and serve it.
	connKey2, err := Mount(root, "NimboDiscTest", "", hydrate, list)
	if err != nil {
		t.Fatalf("re-Mount after Disconnect: %v", err)
	}
	defer Unmount(root, connKey2)
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read after reconnect: %v", err)
	}
	if len(data) != 8 {
		t.Errorf("read %d bytes after reconnect, want 8", len(data))
	}
	if s := placeholderStateOf(t, file); s&cfPlaceholderStatePlaceholder == 0 {
		t.Errorf("keep.bin no longer a placeholder after reconnect (state=0x%02x)", s)
	}
}
