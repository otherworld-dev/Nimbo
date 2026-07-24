//go:build windows

package watch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestWindowsRecursiveWatch exercises the real ReadDirectoryChangesW path: a
// file created deep in the tree must be reported (by absolute path) to the sync
// callback, proving the recursive watch + FILE_NOTIFY_INFORMATION parsing work.
func TestWindowsRecursiveWatch(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var batches [][]string
	gotChange := make(chan struct{}, 16)
	syncFn := func(_ context.Context, changed []string) error {
		mu.Lock()
		batches = append(batches, changed)
		mu.Unlock()
		if changed != nil { // a real change batch, not the startup/poll full pass
			select {
			case gotChange <- struct{}{}:
			default:
			}
		}
		return nil
	}

	done := make(chan struct{})
	go func() {
		_ = Run(ctx, Options{Root: root, Debounce: 150 * time.Millisecond}, syncFn)
		close(done)
	}()

	time.Sleep(400 * time.Millisecond) // let startup sync run and the watch arm

	target := filepath.Join(sub, "c.txt")
	if err := os.WriteFile(target, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case <-gotChange:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not report the change within 5s")
	}

	mu.Lock()
	found := false
	for _, b := range batches {
		for _, p := range b {
			if strings.Contains(strings.ToLower(p), "c.txt") {
				found = true
			}
		}
	}
	mu.Unlock()
	if !found {
		t.Fatalf("changed path %q not reported; batches=%v", target, batches)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("watcher did not stop after cancel")
	}
}
