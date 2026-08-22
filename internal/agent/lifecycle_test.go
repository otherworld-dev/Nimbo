package agent

import (
	"testing"
	"time"

	"github.com/otherworld/nimbo/internal/config"
)

// A pair mutation before Run must not start a watcher (there is no run context
// to derive it from — it could never be stopped) and must not panic. This is a
// real pre-Run path: mountSecondaryOnDemand records the account-root pair via
// AddSyncPair before `go eng.Run(...)`, and the mobile facade's AddSyncFolder
// can race Run's startup.
func TestStartWatcherBeforeRunIsInertNoPanic(t *testing.T) {
	e := &Engine{} // constructed but Run not called: runCtx nil, maps nil
	e.startWatcher(Pair{LocalDir: t.TempDir(), RemoteRoot: "Docs"})
	e.watchMu.Lock()
	n := len(e.watchers)
	e.watchMu.Unlock()
	if n != 0 {
		t.Fatalf("pre-Run startWatcher must be inert, found %d watcher(s)", n)
	}
}

// The two trigger kinds must stay distinct: TriggerSync fires the cheap
// remote-delta pass, TriggerFullSync the local-walking reconcile.
// mobile.Client.SyncNow routes through TriggerFullSync because Android's inotify
// does not see writes made by other apps to shared storage — a manual "Sync now"
// wired to the delta trigger could never upload a file the watcher missed.
//
// Characterisation coverage: this plumbing had no tests, and the two maps are
// easy to transpose in a refactor.
func TestTriggerFullSyncSignalsOnlyTheFullTrigger(t *testing.T) {
	full := make(chan struct{}, 1)
	delta := make(chan struct{}, 1)
	e := &Engine{
		triggers:     map[string]chan struct{}{"pair": delta},
		triggersFull: map[string]chan struct{}{"pair": full},
	}

	e.TriggerFullSync()

	select {
	case <-full:
	default:
		t.Fatal("TriggerFullSync did not signal the full-pass trigger")
	}
	select {
	case <-delta:
		t.Fatal("TriggerFullSync also signalled the remote-delta trigger")
	default:
	}
}

func TestTriggerSyncSignalsOnlyTheDeltaTrigger(t *testing.T) {
	full := make(chan struct{}, 1)
	delta := make(chan struct{}, 1)
	e := &Engine{
		triggers:     map[string]chan struct{}{"pair": delta},
		triggersFull: map[string]chan struct{}{"pair": full},
	}

	e.TriggerSync()

	select {
	case <-delta:
	default:
		t.Fatal("TriggerSync did not signal the remote-delta trigger")
	}
	select {
	case <-full:
		t.Fatal("TriggerSync also signalled the full-pass trigger")
	default:
	}
}

func TestDrainWatchersWaitsForDoneChannels(t *testing.T) {
	quick := make(chan struct{})
	slow := make(chan struct{})
	e := &Engine{watchDone: map[string]chan struct{}{"a": quick, "b": slow}}
	go func() {
		close(quick)
		time.Sleep(50 * time.Millisecond)
		close(slow)
	}()
	start := time.Now()
	e.drainWatchers(5 * time.Second)
	if d := time.Since(start); d < 40*time.Millisecond {
		t.Fatalf("drain returned in %v — did not wait for in-flight watchers", d)
	}
}

func TestDrainWatchersIsBounded(t *testing.T) {
	wedged := make(chan struct{}) // never closes: a wedged sync pass
	e := &Engine{watchDone: map[string]chan struct{}{"a": wedged}}
	start := time.Now()
	e.drainWatchers(100 * time.Millisecond)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("drain took %v — bound not honoured", d)
	}
}

// After Run exits nothing owns the store handle: a straggling sync pass that
// lazily reopened it would leak the DB until process exit. closeStoreFinal
// must make later getStore calls fail instead of reopening.
func TestGetStoreRefusesAfterFinalClose(t *testing.T) {
	tmp := t.TempDir()
	e := &Engine{dirs: config.Dirs{Config: tmp, Data: tmp}}
	if _, err := e.getStore(); err != nil {
		t.Fatalf("first getStore should open the store: %v", err)
	}
	e.closeStoreFinal()
	if _, err := e.getStore(); err == nil {
		t.Fatal("getStore after closeStoreFinal must error, not lazily reopen the DB")
	}
}
