package watch

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestPushRoutesToOnPush verifies an External (push) trigger runs OnPush — the
// remote-delta reconcile — rather than a full SyncFunc pass.
func TestPushRoutesToOnPush(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ext := make(chan struct{}, 1)
	var mu sync.Mutex
	syncCalls, pushCalls := 0, 0
	syncFn := func(_ context.Context, _ []string) error {
		mu.Lock()
		syncCalls++
		mu.Unlock()
		return nil
	}
	onPush := func(_ context.Context) error {
		mu.Lock()
		pushCalls++
		mu.Unlock()
		return nil
	}

	done := make(chan struct{})
	go func() {
		_ = Run(ctx, Options{Root: root, Debounce: 100 * time.Millisecond, External: ext, OnPush: onPush}, syncFn)
		close(done)
	}()

	time.Sleep(300 * time.Millisecond) // let the startup full sync run
	mu.Lock()
	startupSync := syncCalls
	mu.Unlock()

	ext <- struct{}{} // simulate a notify_push

	deadline := time.After(3 * time.Second)
	for {
		mu.Lock()
		pc := pushCalls
		mu.Unlock()
		if pc >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("OnPush was not called after a push trigger")
		case <-time.After(40 * time.Millisecond):
		}
	}

	time.Sleep(200 * time.Millisecond) // give any stray SyncFunc a chance to fire
	mu.Lock()
	defer mu.Unlock()
	if pushCalls != 1 {
		t.Fatalf("pushCalls = %d, want 1", pushCalls)
	}
	if syncCalls != startupSync {
		t.Fatalf("push must not trigger a full SyncFunc: syncCalls %d -> %d", startupSync, syncCalls)
	}
}

// TestPollUsesRemoteDelta verifies that polls run the fast remote-delta (OnPush)
// rather than a full SyncFunc while a full pass isn't yet due — so a push never
// waits behind a ~30s walk every poll. Only startup should be a full SyncFunc.
func TestPollUsesRemoteDelta(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	syncCalls, pushCalls := 0, 0
	syncFn := func(_ context.Context, _ []string) error {
		mu.Lock()
		syncCalls++
		mu.Unlock()
		return nil
	}
	onPush := func(_ context.Context) error {
		mu.Lock()
		pushCalls++
		mu.Unlock()
		return nil
	}

	done := make(chan struct{})
	go func() {
		_ = Run(ctx, Options{
			Root: root, PollInterval: 80 * time.Millisecond,
			FullSyncEvery: time.Hour, OnPush: onPush,
		}, syncFn)
		close(done)
	}()

	time.Sleep(600 * time.Millisecond) // ~7 poll ticks
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if syncCalls != 1 {
		t.Fatalf("only startup should be a full SyncFunc; got %d", syncCalls)
	}
	if pushCalls < 3 {
		t.Fatalf("polls should run the remote-delta; got %d pushCalls", pushCalls)
	}
}

// TestOverflowForcesFullSync verifies a watcher buffer-overflow signal triggers a
// prompt full local scan (not a remote-delta), so lost local paths are recovered
// without waiting for the hourly full pass.
func TestOverflowForcesFullSync(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan string, 4)

	var mu sync.Mutex
	fullCalls, pushCalls := 0, 0
	syncFn := func(_ context.Context, changed []string) error {
		mu.Lock()
		if changed == nil {
			fullCalls++
		}
		mu.Unlock()
		return nil
	}
	onPush := func(_ context.Context) error {
		mu.Lock()
		pushCalls++
		mu.Unlock()
		return nil
	}

	done := make(chan struct{})
	go func() {
		_ = runLoop(ctx, Options{Root: t.TempDir(), Debounce: 40 * time.Millisecond, OnPush: onPush, FullSyncEvery: time.Hour}, syncFn, events)
		close(done)
	}()

	time.Sleep(150 * time.Millisecond) // startup full sync
	mu.Lock()
	startup := fullCalls
	mu.Unlock()

	events <- overflowSignal

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		fc := fullCalls
		mu.Unlock()
		if fc > startup {
			break
		}
		select {
		case <-deadline:
			t.Fatal("overflow did not trigger a full sync")
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if pushCalls != 0 {
		t.Fatalf("overflow must be a full local scan, not a remote-delta; pushCalls=%d", pushCalls)
	}
}
