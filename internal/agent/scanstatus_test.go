package agent

import (
	"sync"
	"testing"
	"time"
)

// recorder returns an Engine whose status emits are captured, plus a func to
// read the captured list safely.
func recorder() (*Engine, func() []string) {
	var mu sync.Mutex
	var got []string
	e := &Engine{}
	e.onStatus = func(s string) {
		mu.Lock()
		got = append(got, s)
		mu.Unlock()
	}
	return e, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

func setScanTiming(t *testing.T, quiet, tick time.Duration) {
	t.Helper()
	pq, pt := scanQuietPeriod, scanTickInterval
	scanQuietPeriod, scanTickInterval = quiet, tick
	t.Cleanup(func() { scanQuietPeriod, scanTickInterval = pq, pt })
}

// A full pass runs every 15s when notify_push is unavailable, and most of those
// passes are fast. Narrating the stages of a scan that finishes in milliseconds
// would flicker the flyout through five messages every 15 seconds, so nothing is
// reported until the scan has been running long enough to look stuck.
func TestScanStaysQuietOnFastPasses(t *testing.T) {
	setScanTiming(t, time.Hour, time.Nanosecond)
	e, emitted := recorder()

	e.scanBegin()
	e.scanPhase("Checking server…")
	e.scanTick("Checking server… 12 folders")
	e.scanPhase("Checking your files…")
	e.scanTick("Checking your files… 40")

	if n := len(emitted()); n != 0 {
		t.Fatalf("a fast pass must stay silent, got %d emit(s): %v", n, emitted())
	}
}

// Once the scan is demonstrably slow, every stage transition is reported — those
// carry the most information and the next tick may be a whole interval away.
func TestScanPhaseEmitsPastQuietPeriod(t *testing.T) {
	setScanTiming(t, 0, time.Hour)
	e, emitted := recorder()

	e.scanBegin()
	e.scanPhase("Checking server…")
	e.scanPhase("Checking your files…")
	e.scanPhase("Comparing changes…")

	got := emitted()
	want := []string{"Checking server…", "Checking your files…", "Comparing changes…"}
	if len(got) != len(want) {
		t.Fatalf("got %d emit(s) %v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("emit %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// The scan callbacks fire once per directory/entry — thousands of times on a big
// tree — and every emit crosses into the GUI as an event.
func TestScanTickThrottlesRapidUpdates(t *testing.T) {
	setScanTiming(t, 0, time.Hour)
	e, emitted := recorder()

	e.scanBegin()
	for i := 0; i < 1000; i++ {
		e.scanTick("Checking your files…")
	}

	if n := len(emitted()); n != 1 {
		t.Fatalf("1000 rapid ticks produced %d emit(s), want exactly 1", n)
	}
}

// A phase change restarts the throttle window, so the stage's first tick doesn't
// immediately re-report what the phase message just said.
func TestScanPhaseRestartsTheThrottleWindow(t *testing.T) {
	setScanTiming(t, 0, time.Hour)
	e, emitted := recorder()

	e.scanBegin()
	e.scanPhase("Checking server…")
	e.scanTick("Checking server… 1 folders")

	if n := len(emitted()); n != 1 {
		t.Fatalf("tick right after a phase change should be throttled, got %d emit(s)", n)
	}
}

// A suppressed phase must not consume the throttle window — otherwise the first
// tick after the quiet period would be swallowed too, and a slow stage could go
// far longer than one interval with nothing on screen.
func TestSuppressedPhaseDoesNotConsumeThrottleWindow(t *testing.T) {
	setScanTiming(t, time.Hour, time.Hour)
	e, emitted := recorder()

	e.scanBegin()
	e.scanPhase("Checking server…") // suppressed: inside the quiet period

	setScanTiming(t, 0, time.Hour) // the scan is now demonstrably slow
	e.scanTick("Checking server… 900 folders")

	if n := len(emitted()); n != 1 {
		t.Fatalf("first tick after the quiet period should emit, got %d", n)
	}
}

// The throttle deliberately has NO trailing timer. A deferred emit could fire
// after computePlan returned and clobber the "Syncing…"/"Up to date" that
// applyPlan sets straight afterwards, wedging the flyout on a scan message.
func TestScanTickLeavesNoTrailingEmit(t *testing.T) {
	setScanTiming(t, 0, 20*time.Millisecond)
	e, emitted := recorder()

	e.scanBegin()
	for i := 0; i < 50; i++ {
		e.scanTick("Checking your files…")
	}
	settled := len(emitted())

	time.Sleep(100 * time.Millisecond) // several intervals with no further calls

	if n := len(emitted()); n != settled {
		t.Fatalf("a trailing emit fired after ticking stopped: %d -> %d", settled, n)
	}
}

// RemoteScan's Progress hook is documented as called from worker goroutines, so
// the throttle must be safe under concurrent use (run with -race).
func TestScanTickIsConcurrencySafe(t *testing.T) {
	setScanTiming(t, 0, time.Millisecond)
	e, emitted := recorder()

	e.scanBegin()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				e.scanTick("Checking server…")
			}
		}()
	}
	wg.Wait()

	if len(emitted()) == 0 {
		t.Fatal("expected at least one emit from concurrent ticks")
	}
}
