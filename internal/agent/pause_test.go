package agent

import (
	"testing"
	"time"
)

// A background account started while the app is paused must start paused
// too: the tray shows one Pause for every account.
func TestCopyPauseFromCarriesAManualPause(t *testing.T) {
	src, dst := &Engine{}, &Engine{}
	src.SetPaused(true)

	dst.CopyPauseFrom(src)

	if !dst.Paused() {
		t.Fatal("the copy is not paused")
	}
	if got := dst.PauseState(); got.Reason != "manual" {
		t.Errorf("reason = %q, want manual", got.Reason)
	}
}

// A timed pause keeps its end time, so both accounts resume together.
func TestCopyPauseFromCarriesATimedPause(t *testing.T) {
	src, dst := &Engine{}, &Engine{}
	src.PauseFor(time.Hour)

	dst.CopyPauseFrom(src)

	if !dst.Paused() {
		t.Fatal("the copy is not paused")
	}
	if a, b := src.PauseState(), dst.PauseState(); a != b {
		t.Errorf("copy = %+v, want %+v", b, a)
	}
}

// Copying from an engine that isn't paused clears a pause the copy had, and
// doesn't touch its quiet hours, which each engine loads from settings.
func TestCopyPauseFromLeavesTheScheduleAlone(t *testing.T) {
	src, dst := &Engine{}, &Engine{}
	dst.SetPaused(true)
	sched := PauseSchedule{Enabled: true, FromMin: 22 * 60, ToMin: 7 * 60}
	dst.SetPauseSchedule(sched)

	dst.CopyPauseFrom(src)

	dst.mu.Lock()
	manual, until, got := dst.paused, dst.pauseUntil, dst.schedule
	dst.mu.Unlock()
	if manual || !until.IsZero() {
		t.Errorf("paused = %v, until = %v; want the pause cleared", manual, until)
	}
	if got != sched {
		t.Errorf("schedule = %+v, want %+v", got, sched)
	}
}
