package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

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

// laneStatus is the status line while the lane holds work, or "".
func (e *Engine) laneStatus() string {
	l := e.currentLane()
	if l == nil {
		return ""
	}
	switch n := l.count(); n {
	case 0:
		return ""
	case 1:
		return "Syncing 1 large file…"
	default:
		return fmt.Sprintf("Syncing %d large files…", n)
	}
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
	j := &laneJob{pk: pk, rel: a.Path, abs: abs, size: size}
	j.run = func(ctx context.Context) error { return e.runLaneTransfer(ctx, p, pk, a, scan) }
	j.done = func(err error) { e.laneDone(p, pk, a, scan, err) }

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
func (e *Engine) runLaneTransfer(ctx context.Context, p Pair, pk string, a engine.Action, scan map[string]engine.RemoteState) error {
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
		OnProgress: func(_ engine.Action, delta int64) { e.progBytes.Add(delta) },
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
// stopped it owns what happens next. A finished one gets a pass's
// bookkeeping, and on success a nudge, so a quick pass re-checks the file:
// a newer version saved meanwhile goes next. A failure isn't nudged (it could
// loop); the next scheduled pass retries it, as it does a failed transfer in
// a pass.
func (e *Engine) laneDone(p Pair, pk string, a engine.Action, scan map[string]engine.RemoteState, err error) {
	abs := filepath.Join(p.LocalDir, filepath.FromSlash(a.Path))
	if errors.Is(err, errLaneStopped) {
		e.markInflight(abs, false)
		if e.lockWarn != nil {
			e.lockWarn.retake(abs)
		}
		slog.Info("large transfer stopped", "path", a.Path, "op", a.Kind.String())
	} else {
		e.finishAction(p, pk, scan, a, err)
		if err == nil {
			e.nudgePath(p.LocalDir, p.RemoteRoot, abs)
		}
	}
	e.progEnd()
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
