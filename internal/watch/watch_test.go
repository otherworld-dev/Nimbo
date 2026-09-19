package watch

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
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

// A nudged path is synced exactly like a local change the watcher saw: an
// upload put off while a program had the file open is nudged once it closes,
// since closing it may not write, and so may not raise a watcher event.
func TestNudgedPathsSyncLikeLocalChanges(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	nudge := make(chan string, 1)
	got := make(chan []string, 4)
	syncFn := func(_ context.Context, changed []string) error {
		if changed != nil {
			got <- changed
		}
		return nil
	}
	go func() {
		_ = runLoop(ctx, Options{Root: t.TempDir(), Debounce: 20 * time.Millisecond, Nudge: nudge}, syncFn, make(chan string))
	}()
	nudge <- `C:\Sync\archive.pst`
	select {
	case changed := <-got:
		if len(changed) != 1 || changed[0] != `C:\Sync\archive.pst` {
			t.Fatalf("synced %v, want the nudged path", changed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a nudged path was never synced")
	}
}

// A quick sync of local changes that fails (the server unreachable, say) lost
// its paths: nothing re-queued them, and the full pass that would have found
// them again is itself held back by the failure backoff. They are retried.
func TestAFailedChangeSyncIsRetried(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan string, 1)
	var mu sync.Mutex
	var changeCalls [][]string
	syncFn := func(_ context.Context, changed []string) error {
		if changed == nil {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		changeCalls = append(changeCalls, changed)
		if len(changeCalls) == 1 {
			return errors.New("server unreachable")
		}
		return nil
	}
	go func() {
		_ = runLoop(ctx, Options{Root: t.TempDir(), Debounce: 20 * time.Millisecond, RetryChanges: 50 * time.Millisecond}, syncFn, events)
	}()
	events <- `C:\Sync\new.txt`
	deadline := time.After(3 * time.Second)
	for {
		mu.Lock()
		n := len(changeCalls)
		var last []string
		if n > 0 {
			last = changeCalls[n-1]
		}
		mu.Unlock()
		if n >= 2 {
			if len(last) != 1 || last[0] != `C:\Sync\new.txt` {
				t.Fatalf("retried %v, want the failed batch's path", last)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("a failed change sync was never retried (%d calls)", n)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Retrying a change that keeps failing (a path the server refuses) must not
// hold back syncing FROM the server: each retry fed the same failure streak
// that pauses pushes and polls, so one bad path could keep them waiting for
// hours. Change retries keep their own count.
func TestFailingChangeRetriesDoNotHoldBackPushes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan string, 1)
	ext := make(chan struct{}, 1)
	var changes, pushes atomic.Int32
	syncFn := func(_ context.Context, changed []string) error {
		if changed == nil {
			return nil
		}
		changes.Add(1)
		return errors.New("the server refuses this path")
	}
	onPush := func(context.Context) error { pushes.Add(1); return nil }
	go func() {
		_ = runLoop(ctx, Options{Root: t.TempDir(), Debounce: 10 * time.Millisecond, PollInterval: time.Hour,
			OnPush: onPush, External: ext, RetryChanges: 10 * time.Millisecond}, syncFn, events)
	}()
	events <- `C:\Sync\refused.txt`
	deadline := time.After(2 * time.Second)
	for changes.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("only %d change attempts", changes.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}
	ext <- struct{}{}
	deadline = time.After(2 * time.Second)
	for pushes.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("a server push was held back by failing change retries")
		case <-time.After(5 * time.Millisecond):
		}
	}
}
