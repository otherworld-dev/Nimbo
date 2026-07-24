package agent

import (
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/otherworld/nimbo/internal/config"
)

func TestStateResetTripwire(t *testing.T) {
	d := config.Dirs{Config: t.TempDir()}.WithAccount("acct1")
	var toasts []string
	e := &Engine{dirs: d}
	e.onToast = func(title, message, link string) { toasts = append(toasts, title) }

	// Never-synced pair entering a clone: silence.
	e.warnIfStateReset("pk1", Pair{LocalDir: `E:\X`, RemoteRoot: "X"})
	if len(toasts) != 0 {
		t.Fatalf("fresh pair must not warn: %v", toasts)
	}

	// Pair completes a clone -> marker recorded.
	e.markPairSynced("pk1")
	hist, err := d.LoadSyncHistory()
	if err != nil || !hist["pk1"] {
		t.Fatalf("marker not recorded: %v %v", hist, err)
	}

	// Same pair entering a clone again = state DB vanished: warn, toast once.
	e.warnIfStateReset("pk1", Pair{LocalDir: `E:\X`, RemoteRoot: "X"})
	e.warnIfStateReset("pk1", Pair{LocalDir: `E:\X`, RemoteRoot: "X"})
	if len(toasts) != 1 {
		t.Fatalf("want exactly one toast, got %v", toasts)
	}

	// Marker I/O is best-effort: a broken Dirs must not panic anything.
	broken := &Engine{dirs: config.Dirs{Config: `Z:\definitely\not\here`}}
	broken.markPairSynced("pk1")
	broken.warnIfStateReset("pk1", Pair{})
}

func TestMarkPairSyncedOnceBackfills(t *testing.T) {
	d := config.Dirs{Config: t.TempDir()}.WithAccount("a")
	e := &Engine{dirs: d}
	e.markPairSyncedOnce("pk1")
	hist, err := d.LoadSyncHistory()
	if err != nil || !hist["pk1"] {
		t.Fatalf("backfill did not record: %v %v", hist, err)
	}
	// Second call must be a run-cached no-op: remove the file and confirm the
	// steady-state path performs no further file I/O.
	if err := os.Remove(d.SyncHistoryFile()); err != nil {
		t.Fatal(err)
	}
	e.markPairSyncedOnce("pk1")
	if _, err := os.Stat(d.SyncHistoryFile()); err == nil {
		t.Fatal("second call re-wrote the file; want cached no-op")
	}
}

func TestMarkPairSyncedConcurrent(t *testing.T) {
	d := config.Dirs{Config: t.TempDir()}.WithAccount("a")
	e := &Engine{dirs: d}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			e.markPairSynced(fmt.Sprintf("pk%d", n))
		}(i)
	}
	wg.Wait()
	hist, err := d.LoadSyncHistory()
	if err != nil || len(hist) != 8 {
		t.Fatalf("lost updates: got %d markers (%v), err %v", len(hist), hist, err)
	}
}
