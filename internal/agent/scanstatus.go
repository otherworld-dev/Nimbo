package agent

import (
	"strconv"
	"time"
)

var (
	// scanQuietPeriod is how long a scan must run before it starts narrating
	// itself. A full pass runs every 15s when notify_push is unavailable
	// (pollInterval), and a warm pass finishes in milliseconds thanks to the
	// etag prune — narrating those would flicker the flyout through five
	// messages every 15 seconds. Below this threshold the scan says nothing and
	// the "Scanning…" SyncOnce already set stays on screen, exactly as before.
	scanQuietPeriod = 1500 * time.Millisecond

	// scanTickInterval bounds how often a heartbeat within a stage reaches the
	// UI. The scan callbacks fire once per directory (RemoteScan) or per entry
	// (LocalScan) — thousands of times on a large tree — and every status emit
	// crosses into the GUI as an event.
	scanTickInterval = 250 * time.Millisecond
)

// scanBegin marks the start of a scan pass, restarting the quiet period. Call it
// once at the top of a full plan computation.
func (e *Engine) scanBegin() {
	e.scanMu.Lock()
	e.scanStart = time.Now()
	e.scanLastEmit = time.Time{}
	e.scanMu.Unlock()
}

// scanPhase reports that the scan moved to a new stage. Past the quiet period it
// always emits — stage transitions carry the most information and the next tick
// may be a whole interval away — and it restarts the throttle window so the
// stage's first tick doesn't immediately re-report the same thing.
//
// A suppressed phase does NOT touch the throttle window. Consuming it would
// swallow the first tick after the quiet period too, which is precisely when the
// user has been staring at an unchanging message the longest.
func (e *Engine) scanPhase(text string) {
	e.scanMu.Lock()
	if time.Since(e.scanStart) < scanQuietPeriod {
		e.scanMu.Unlock()
		return
	}
	e.scanLastEmit = time.Now()
	e.scanMu.Unlock()
	e.status(text)
}

// commas renders n with thousand separators ("34120" -> "34,120"). Counts in the
// status line are read at a glance from a narrow, truncating label; a bare run of
// six digits is not.
func commas(n int) string {
	s := strconv.Itoa(n)
	sign := ""
	if n < 0 {
		sign, s = "-", s[1:]
	}
	head := len(s) % 3
	if head == 0 {
		head = 3
	}
	out := s[:head]
	for i := head; i < len(s); i += 3 {
		out += "," + s[i:i+3]
	}
	return sign + out
}

// scanTick reports progress within the current stage. It emits at most once per
// scanTickInterval, and only once the scan has outlived the quiet period.
//
// There is deliberately NO trailing timer. A deferred emit could fire after
// computePlan has returned and overwrite the "Syncing…" or "Up to date" that
// applyPlan sets immediately afterwards, wedging the flyout on a scan message
// for the rest of the pass. Losing the last tick of a stage costs nothing — a
// scanPhase call follows right after it.
//
// Safe for concurrent use: RemoteScan's Progress hook runs on worker goroutines.
func (e *Engine) scanTick(text string) {
	e.scanMu.Lock()
	if time.Since(e.scanStart) < scanQuietPeriod || time.Since(e.scanLastEmit) < scanTickInterval {
		e.scanMu.Unlock()
		return
	}
	e.scanLastEmit = time.Now()
	e.scanMu.Unlock()
	e.status(text)
}
