package main

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Two callers wanting the same icon at once (opening an app while the icon
// migration regenerates it) must share one fetch. When both fetched, the second
// rename onto the finished icon failed with "Access is denied" and that was
// handled as a failed fetch, writing the brand icon over the good one (#743).
func TestEnsureIconFileSharesConcurrentFetch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "files.3.ico")
	var calls atomic.Int32
	fetch := func(p string) error {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond) // hold the fetch open so the callers overlap
		return os.WriteFile(p, []byte("real"), 0o600)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ensureIconFile(path, fetch)
		}()
	}
	wg.Wait()

	if n := calls.Load(); n != 1 {
		t.Errorf("fetch ran %d times, want 1", n)
	}
	if b, _ := os.ReadFile(path); string(b) != "real" {
		t.Errorf("icon = %q, want the fetched icon", b)
	}
	if _, err := os.Stat(path + ".fallback"); err == nil {
		t.Error("fallback marker left behind a real icon")
	}
}

// A failed fetch still leaves an icon at the path (the shortcut points at it)
// and marks it as the fallback, so the next call tries the server again.
func TestEnsureIconFileFallbackThenRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "files.3.ico")
	ensureIconFile(path, func(string) error { return errors.New("server down") })
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("no icon after a failed fetch: %v", err)
	}
	if _, err := os.Stat(path + ".fallback"); err != nil {
		t.Fatal("failed fetch not marked as fallback")
	}

	ensureIconFile(path, func(p string) error { return os.WriteFile(p, []byte("real"), 0o600) })
	if b, _ := os.ReadFile(path); string(b) != "real" {
		t.Errorf("icon = %q after the retry, want the fetched icon", b)
	}
	if _, err := os.Stat(path + ".fallback"); err == nil {
		t.Error("fallback marker kept after a successful retry")
	}
}
