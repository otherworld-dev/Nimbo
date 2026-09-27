package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/otherworld/nimbo/internal/engine"
	"github.com/otherworld/nimbo/internal/transfer"
)

// The glue between sync passes and the long-transfer lane (lane.go, Deck
// #702): a planned upload or download becomes a lane job that runs the same
// transfer code a pass runs, with the same bookkeeping when it ends.

// transferLane returns the engine's lane, creating it on first use.
func (e *Engine) transferLane() *lane {
	paused := e.Paused() // before watchMu: Paused takes e.mu
	e.watchMu.Lock()
	defer e.watchMu.Unlock()
	if e.tl == nil {
		e.tl = newLane(paused)
		e.tl.onChange = func() {
			if f := e.onLane; f != nil {
				f()
			}
		}
	}
	return e.tl
}

// currentLane returns the lane if there is one, without creating it.
func (e *Engine) currentLane() *lane {
	e.watchMu.Lock()
	defer e.watchMu.Unlock()
	return e.tl
}

// closeLane stops every lane transfer and waits for them (engine shutdown).
// Nothing is saved: what was unsent is still unsent, so the next pass plans
// it again and it resumes from the server's chunks or the .nimbo-part.
func (e *Engine) closeLane() {
	e.watchMu.Lock()
	l := e.tl
	e.tl = nil
	e.watchMu.Unlock()
	if l != nil {
		l.close()
	}
}

// laneStatus is the status line while the lane holds work, or "". Set-aside
// files are named only when nothing is transferring: they are unsynced, so
// "Up to date" would be untrue, but they are not being worked on either.
func (e *Engine) laneStatus() string {
	l := e.currentLane()
	if l == nil {
		return ""
	}
	active, parked := l.count(), l.parkedCount()
	switch {
	case active == 1:
		return "Syncing 1 large file…"
	case active > 1:
		return fmt.Sprintf("Syncing %d large files…", active)
	case parked == 1:
		return "1 large file set aside"
	case parked > 1:
		return fmt.Sprintf("%d large files set aside", parked)
	}
	return ""
}

// sendToLane hands a planned transfer to the lane. False when the lane won't
// take it (shutting down, or it already holds the path); the pass keeps it.
func (e *Engine) sendToLane(p Pair, pk string, a engine.Action, size int64, remote map[string]engine.RemoteState) bool {
	abs := filepath.Join(p.LocalDir, filepath.FromSlash(a.Path))
	// The job outlives the pass's remote map; keep just this file's entry
	// (download ETag/FileID fallback, share-root flag, read-only flag).
	scan := map[string]engine.RemoteState{}
	if r, ok := remote[a.Path]; ok {
		scan[a.Path] = r
	}
	j := &laneJob{pk: pk, rel: a.Path, abs: abs, dir: p.LocalDir, up: a.Kind == engine.ActUpload, size: size}
	// The job's share of the progress burst ends once: when it is set aside,
	// or when it leaves the lane, whichever comes first. A set-aside job can
	// leave (Resume, its time, a stop) before or after onPark has run.
	var endOnce sync.Once
	end := func() { endOnce.Do(e.progEnd) }
	// left is set by done, before laneDone runs, so a late onPark (the lane
	// can call it AFTER done: a concurrent unpark/stop/timer can take the job
	// out from under exec() between it releasing the lock and it calling
	// onPark) sees the job has already left and does nothing. Without this, a
	// late onPark's laneParked would clear the inflight/lock-warning state of
	// this path after a fresh pass has already re-planned and re-sent it —
	// clearing the NEW job's bookkeeping, not this one's.
	var left atomic.Bool
	j.run = func(ctx context.Context) error { return e.runLaneTransfer(ctx, p, pk, a, scan, &j.sent) }
	j.onPark = func() {
		if left.Load() {
			return
		}
		e.laneParked(abs, a, end)
	}
	j.done = func(err error) {
		left.Store(true)
		e.laneDone(p, pk, a, scan, err, end)
	}

	e.markInflight(abs, true) // syncing icon from hand-off, not just while it runs
	e.progStart(1, size)      // keeps the flyout's progress up while it waits and runs
	if !e.transferLane().add(j) {
		e.markInflight(abs, false)
		e.progEnd()
		return false
	}
	slog.Info("large transfer moved out of the pass", "path", a.Path, "op", a.Kind.String(), "size", size)
	return true
}

// runLaneTransfer runs one lane job's transfer through a one-action executor
// configured like a pass's, so retries, chunk resume, torn-file and in-use
// checks and the baseline write are exactly a pass's.
func (e *Engine) runLaneTransfer(ctx context.Context, p Pair, pk string, a engine.Action, scan map[string]engine.RemoteState, sent *atomic.Int64) error {
	st, err := e.getStore()
	if err != nil {
		return err
	}
	e.beginAction(p, a)
	var got error
	reported := false
	ex := &transfer.Executor{
		Client:     e.client,
		State:      st,
		PairKey:    pk,
		LocalRoot:  p.LocalDir,
		RemoteRoot: p.RemoteRoot,
		Remote:     scan,
		Escaper:    e.escaper.Load(),
		Workers:    1,
		Policy:     e.policy,
		OnProgress: func(_ engine.Action, delta int64) { e.progBytes.Add(delta); sent.Add(delta) },
		OnEvent:    func(_ engine.Action, aerr error) { got, reported = aerr, true },
	}
	if _, err := ex.Run(ctx, []engine.Action{a}); err != nil {
		return err
	}
	if !reported { // cancelled before it started
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("transfer did not start")
	}
	return got
}

// laneDone ends a lane job. A stopped job is put back quietly: whatever
// stopped it owns what happens next. A set-aside job that came back is
// nudged, so a fresh pass plans it from what is on disk and on the server
// now; the action it carried may be hours stale. A finished one gets a
// pass's bookkeeping, and on success a nudge, so a quick pass re-checks the
// file: a newer version saved meanwhile goes next. A failure isn't nudged (it
// could loop); the next scheduled pass retries it, as it does a failed
// transfer in a pass. end ends the job's share of progress, once.
func (e *Engine) laneDone(p Pair, pk string, a engine.Action, scan map[string]engine.RemoteState, err error, end func()) {
	abs := filepath.Join(p.LocalDir, filepath.FromSlash(a.Path))
	switch {
	case errors.Is(err, errLaneUnparked):
		slog.Info("large transfer back from being set aside", "path", a.Path, "op", a.Kind.String())
		e.nudgePath(p.LocalDir, p.RemoteRoot, abs)
	case errors.Is(err, errLaneStopped):
		e.markInflight(abs, false)
		if e.lockWarn != nil {
			e.lockWarn.retake(abs)
		}
		slog.Info("large transfer stopped", "path", a.Path, "op", a.Kind.String())
	default:
		e.finishAction(p, pk, scan, a, err)
		if err == nil {
			e.nudgePath(p.LocalDir, p.RemoteRoot, abs)
		}
	}
	end()
	e.laneSettled()
}

// laneParked is a job being set aside: no longer syncing, so its syncing icon
// goes, it stops counting toward the flyout's progress, and the status line
// says a file is set aside rather than syncing.
func (e *Engine) laneParked(abs string, a engine.Action, end func()) {
	e.markInflight(abs, false)
	if e.lockWarn != nil {
		e.lockWarn.retake(abs)
	}
	slog.Info("large transfer set aside", "path", a.Path, "op", a.Kind.String())
	end()
	e.laneSettled()
}

// laneSettled refreshes a status line that counts lane transfers after one
// leaves. Only the lane's own lines are replaced: an Error, Offline or a
// pass's "Syncing…" set meanwhile stays.
func (e *Engine) laneSettled() {
	e.diagMu.Lock()
	last := e.lastStatus
	e.diagMu.Unlock()
	if strings.Contains(last, "large file") {
		e.status("Up to date") // status() swaps in the lane's line while it still has work
	}
}

// stopLanePair stops a folder pair's lane transfers and waits for them: the
// folder is being removed or moved, and nothing of it may still be sending.
func (e *Engine) stopLanePair(pk string) {
	if l := e.currentLane(); l != nil {
		l.stop(func(j *laneJob) bool { return j.pk == pk })
	}
}

// stopLaneUnder stops the lane transfers of abs and of anything beneath it,
// and waits for them: the path was blacklisted or deselected.
func (e *Engine) stopLaneUnder(abs string) {
	if l := e.currentLane(); l != nil {
		l.stop(func(j *laneJob) bool { return within(j.abs, abs) })
	}
}

// LaneEntry is one large transfer as the queue view shows it (Deck #702).
type LaneEntry struct {
	LocalDir string // the sync folder it belongs to
	Path     string // pair-relative, slash-separated
	Abs      string // local path; the key the Lane* methods take
	Upload   bool   // else a download
	Size     int64
	Sent     int64     // bytes moved by the current attempt
	State    string    // "running", "waiting", "paused" or "setaside"
	Position int       // 1-based place in the queue, for "waiting" and "paused"
	Until    time.Time // set aside until; zero = until resumed
}

// LaneEntries lists the large transfers: running, then waiting in order,
// then set aside.
func (e *Engine) LaneEntries() []LaneEntry {
	l := e.currentLane()
	if l == nil {
		return nil
	}
	ents := l.entries()
	out := make([]LaneEntry, 0, len(ents))
	for _, en := range ents {
		st := "setaside"
		switch {
		case en.state == laneRunning:
			st = "running"
		case en.state == laneWaiting && en.paused:
			st = "paused"
		case en.state == laneWaiting:
			st = "waiting"
		}
		out = append(out, LaneEntry{LocalDir: en.dir, Path: en.rel, Abs: en.abs, Upload: en.up,
			Size: en.size, Sent: en.sent, State: st, Position: en.pos, Until: en.until})
	}
	return out
}

// LaneSyncFirst moves the waiting large transfer of abs to the front, so it
// starts next. False when abs isn't waiting in the lane.
func (e *Engine) LaneSyncFirst(abs string) bool {
	l := e.currentLane()
	return l != nil && l.first(abs)
}

// LaneSetAside sets the large transfer of abs aside until until (zero: until
// LaneResume). A restart ends it either way. False when the lane doesn't
// hold abs.
func (e *Engine) LaneSetAside(abs string, until time.Time) bool {
	l := e.currentLane()
	return l != nil && l.park(abs, until)
}

// LaneResume brings a set-aside transfer back: a quick pass re-plans it.
// False when abs isn't set aside.
func (e *Engine) LaneResume(abs string) bool {
	l := e.currentLane()
	return l != nil && l.unpark(abs)
}

// SetLaneFunc registers a callback told whenever the lane changes (a file
// joins, starts, moves, is set aside or leaves). Set it before Run.
func (e *Engine) SetLaneFunc(f func()) { e.onLane = f }
