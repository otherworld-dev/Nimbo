package agent

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/otherworld/nimbo/internal/config"
)

// The long-transfer lane (Deck #702). A sync pass plans everything up front
// and a pair's passes run one after another, so a file saved while a 300 GB
// upload was running waited for that upload to end before it could sync.
// Transfers of laneMinBytes or more now leave the pass and run here instead:
// per engine, laneWorkers at a time, first come first served, outliving the
// pass that planned them. The pass ends in seconds and the next one can start.
// Since stage 2 the user can also move a waiting job to the front and set a
// job aside for a while; a set-aside job is still held, so passes keep
// leaving its file alone.
//
// The lane knows nothing about syncing. A job is an identity (pair key and
// pair-relative path, plus the local path for stops by folder), a run that
// performs the transfer, and a done that is told once how it ended. The glue
// that makes jobs out of planned actions is lanejobs.go.

var (
	// laneMinBytes is the size from which a transfer goes to the lane. A
	// variable so tests can use small files.
	laneMinBytes int64 = 64 << 20
	// laneWorkers is how many lane transfers run at once per engine.
	laneWorkers = 2
)

// errLaneStopped is what done hears for a job that left the lane unfinished:
// stopped because its file is being moved or deleted, its folder is going
// away, it was blacklisted, or the engine is shutting down. Not a failure:
// whatever stopped it owns what happens next.
var errLaneStopped = errors.New("stopped")

// errLaneUnparked is what done hears for a set-aside job that came back
// (Resume, or its time ran out). Not a failure and not a stop: the engine
// re-plans the file from scratch, because the plan the job carries can be
// hours stale by now.
var errLaneUnparked = errors.New("back from being set aside")

// laneState is where a job is, for the queue view.
type laneState int

const (
	laneWaiting laneState = iota + 1
	laneRunning
	laneParked // set aside by the user (Deck #702, stage 2)
)

type laneJob struct {
	pk, rel string // identity: pair key + pair-relative path
	abs     string // local path, for stops by folder and the queue view's buttons
	dir     string // the pair's local folder, for the queue view
	up      bool   // an upload (else a download), for the queue view
	size    int64
	run     func(ctx context.Context) error // the transfer; ctx is cancelled to stop it
	done    func(err error)                 // told once, when the job leaves the lane for good
	onPark  func()                          // told when the job is set aside; may be nil

	sent atomic.Int64 // bytes moved by the current attempt; reset at each start

	// Guarded by lane.mu.
	seq      uint64 // order of arrival, so running jobs list in a stable order
	cancel   context.CancelFunc
	ended    chan struct{} // closed once a running attempt has returned and done (if due) ran
	requeue  bool          // cancelled by pause: back to the queue, done not told
	stopped  bool          // cancelled by stop or close: done hears errLaneStopped
	parking  bool          // cancelled by park: to the parked list, done not told
	until    time.Time     // set aside until this; zero = until unpark
	timer    *time.Timer   // runs timerUnpark when until comes
	timerGen uint64        // bumped each time timer is (re)armed; a stale callback checks this and no-ops
}

type lane struct {
	mu      sync.Mutex
	queue   []*laneJob
	running map[*laneJob]bool
	parked  []*laneJob // set aside: still held (passes leave them alone), never dispatched
	paused  bool
	closed  bool
	// stoppedPairs are the pairs stopPair took out of the lane and add
	// refuses, until allowPair: a folder that stopped syncing (removed,
	// moved, held back) takes no new transfers from a pass that was already
	// planning it.
	stoppedPairs map[string]bool
	seq          uint64
	wg           sync.WaitGroup
	// onChange is told, without mu held, whenever a job arrives, starts,
	// moves, is set aside or leaves. Set once, before the lane is used.
	onChange func()
}

func (l *lane) changed() {
	if l.onChange != nil {
		l.onChange()
	}
}

func newLane(paused bool) *lane {
	return &lane{running: make(map[*laneJob]bool), paused: paused}
}

// relUnder reports whether the pair-relative path p is dir or lies beneath it.
// "" is the pair's root, so everything is under it.
func relUnder(p, dir string) bool {
	return dir == "" || p == dir || strings.HasPrefix(p, dir+"/")
}

// add queues j. False when the lane is closed, j's pair has been stopped
// (stopPair), or the lane already holds j's path.
func (l *lane) add(j *laneJob) bool {
	l.mu.Lock()
	if l.closed || l.stoppedPairs[j.pk] || l.holdsLocked(func(o *laneJob) bool { return o.pk == j.pk && o.rel == j.rel }) {
		l.mu.Unlock()
		return false
	}
	l.seq++
	j.seq = l.seq
	l.queue = append(l.queue, j)
	l.dispatchLocked()
	l.mu.Unlock()
	l.changed()
	return true
}

func (l *lane) holdsLocked(match func(*laneJob) bool) bool {
	for _, j := range l.queue {
		if match(j) {
			return true
		}
	}
	for j := range l.running {
		if match(j) {
			return true
		}
	}
	for _, j := range l.parked {
		if match(j) {
			return true
		}
	}
	return false
}

// has reports a job for exactly rel in pair pk: queued, running or set aside.
func (l *lane) has(pk, rel string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.holdsLocked(func(j *laneJob) bool { return j.pk == pk && j.rel == rel })
}

// covers reports a job for rel or anything beneath it: queued, running or
// set aside.
func (l *lane) covers(pk, rel string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.holdsLocked(func(j *laneJob) bool { return j.pk == pk && relUnder(j.rel, rel) })
}

// count is how many jobs are queued or running (set-aside ones are not syncing).
func (l *lane) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.queue) + len(l.running)
}

// parkedCount is how many jobs are set aside.
func (l *lane) parkedCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.parked)
}

func (l *lane) dispatchLocked() {
	for !l.paused && !l.closed && len(l.running) < laneWorkers && len(l.queue) > 0 {
		j := l.queue[0]
		l.queue = l.queue[1:]
		ctx, cancel := context.WithCancel(context.Background())
		j.cancel, j.ended, j.requeue, j.stopped, j.parking = cancel, make(chan struct{}), false, false, false
		j.sent.Store(0) // a resume re-counts the chunks already sent, so start from 0
		l.running[j] = true
		l.wg.Add(1)
		go l.exec(ctx, j)
	}
}

func (l *lane) exec(ctx context.Context, j *laneJob) {
	defer l.wg.Done()
	err := j.run(ctx)
	j.cancel()

	l.mu.Lock()
	delete(l.running, j)
	ended := j.ended
	finished, parked := true, false
	switch {
	case err != nil && j.parking && !l.closed:
		// Set aside mid-transfer. Chunk resume and .nimbo-part mean it
		// picks up later rather than starting over.
		l.parkLocked(j)
		finished, parked = false, true
	case err != nil && j.requeue && !l.closed:
		// Paused mid-transfer: back to the front, to carry on when unpaused.
		l.queue = append([]*laneJob{j}, l.queue...)
		finished = false
	}
	if err != nil && (j.stopped || (finished && (j.parking || j.requeue) && l.closed)) {
		// Stopped, or set aside or paused just as the lane closed (close
		// marked it closed before its stop reached this job): either way a
		// stop, not a failure.
		err = errLaneStopped
	}
	l.dispatchLocked()
	l.mu.Unlock()

	if parked && j.onPark != nil {
		j.onPark()
	}
	if finished {
		j.done(err)
	}
	close(ended)
	l.changed()
}

// pause cancels the running jobs and holds the queue until resume. The
// cancelled jobs go back to the front of the queue; done is not told.
// Set-aside jobs are left as they are.
func (l *lane) pause() {
	l.mu.Lock()
	if l.paused {
		l.mu.Unlock()
		return
	}
	l.paused = true
	for j := range l.running {
		j.requeue = true
		j.cancel()
	}
	l.mu.Unlock()
	l.changed()
}

// resume lets the queue run again. Set-aside jobs stay set aside.
func (l *lane) resume() {
	l.mu.Lock()
	l.paused = false
	l.dispatchLocked()
	l.mu.Unlock()
	l.changed()
}

// stop takes every job match accepts out of the lane: queued ones at once,
// running ones cancelled, set-aside ones dropped (their timer, if any, is
// stopped so it can never unpark them after the fact). It returns once all
// of them have ended and been told (errLaneStopped, or the transfer's own
// result if it finished first), so the caller can move or delete their files.
func (l *lane) stop(match func(*laneJob) bool) {
	l.mu.Lock()
	var dropped []*laneJob
	kept := l.queue[:0]
	for _, j := range l.queue {
		if match(j) {
			dropped = append(dropped, j)
		} else {
			kept = append(kept, j)
		}
	}
	l.queue = kept
	keptParked := l.parked[:0]
	for _, j := range l.parked {
		if match(j) {
			l.invalidateTimerLocked(j)
			dropped = append(dropped, j)
		} else {
			keptParked = append(keptParked, j)
		}
	}
	l.parked = keptParked
	var waits []chan struct{}
	for j := range l.running {
		if match(j) {
			j.stopped, j.requeue, j.parking = true, false, false
			j.cancel()
			waits = append(waits, j.ended)
		}
	}
	l.mu.Unlock()
	for _, j := range dropped {
		j.done(errLaneStopped)
	}
	for _, w := range waits {
		<-w
	}
	l.changed()
}

// stopPair stops every job of pair pk, as stop does, and refuses new ones
// for pk until allowPair(pk). The pair is marked first, so a job added while
// the stop runs is refused rather than missed.
func (l *lane) stopPair(pk string) {
	l.mu.Lock()
	if l.stoppedPairs == nil {
		l.stoppedPairs = map[string]bool{}
	}
	l.stoppedPairs[pk] = true
	l.mu.Unlock()
	l.stop(func(j *laneJob) bool { return j.pk == pk })
}

// allowPair lets pair pk's jobs in again after stopPair.
func (l *lane) allowPair(pk string) {
	l.mu.Lock()
	delete(l.stoppedPairs, pk)
	l.mu.Unlock()
}

// pairStopped reports whether stopPair has stopped pk and allowPair not yet
// let it back.
func (l *lane) pairStopped(pk string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stoppedPairs[pk]
}

// close stops everything and refuses new jobs. It returns once every running
// job has ended.
func (l *lane) close() {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	l.stop(func(*laneJob) bool { return true })
	l.wg.Wait()
}

// findLocked returns the job at the local path abs and where it is. Paths
// compare the way Windows does (config.PathKey).
func (l *lane) findLocked(abs string) (*laneJob, laneState) {
	key := config.PathKey(abs)
	for j := range l.running {
		if config.PathKey(j.abs) == key {
			return j, laneRunning
		}
	}
	for _, j := range l.queue {
		if config.PathKey(j.abs) == key {
			return j, laneWaiting
		}
	}
	for _, j := range l.parked {
		if config.PathKey(j.abs) == key {
			return j, laneParked
		}
	}
	return nil, 0
}

// parkedAt reports whether the job at the local path abs is set aside,
// including a running one on its way there. Cheap enough for Explorer's
// per-item badge queries: one lock and a look through the (tiny) lane.
func (l *lane) parkedAt(abs string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	j, st := l.findLocked(abs)
	return st == laneParked || (st == laneRunning && j.parking)
}

func (l *lane) removeQueuedLocked(j *laneJob) {
	for i, o := range l.queue {
		if o == j {
			l.queue = append(l.queue[:i], l.queue[i+1:]...)
			return
		}
	}
}

// parkLocked puts j on the parked list and starts its timer, if it has one.
func (l *lane) parkLocked(j *laneJob) {
	l.parked = append(l.parked, j)
	l.armLocked(j)
}

// armLocked (re)starts j's set-aside timer, if j.until is set. Each arm bumps
// j.timerGen and the callback captures its own generation, so a timer that
// fires just as park() re-arms the same job with a new time finds a stale
// generation once it gets l.mu and does nothing — see timerUnpark.
func (l *lane) armLocked(j *laneJob) {
	j.timerGen++
	gen := j.timerGen
	if !j.until.IsZero() {
		j.timer = time.AfterFunc(time.Until(j.until), func() { l.timerUnpark(j, gen) })
	} else {
		j.timer = nil
	}
}

// invalidateTimerLocked stops j's timer (if any) and bumps its generation, so
// a callback already blocked on l.mu when this runs is guaranteed stale by
// the time it gets the lock.
func (l *lane) invalidateTimerLocked(j *laneJob) {
	if j.timer != nil {
		j.timer.Stop()
		j.timer = nil
	}
	j.timerGen++
}

// first moves the waiting job at abs to the front of the queue, so it starts
// next. It never interrupts a running job: every resume re-hashes the chunks
// already sent, which is costly on exactly the files that are in the lane.
// False when abs isn't waiting.
func (l *lane) first(abs string) bool {
	l.mu.Lock()
	j, st := l.findLocked(abs)
	if st != laneWaiting {
		l.mu.Unlock()
		return false
	}
	l.removeQueuedLocked(j)
	l.queue = append([]*laneJob{j}, l.queue...)
	l.mu.Unlock()
	l.changed()
	return true
}

// park sets the job at abs aside until until (zero: until unpark). A waiting
// job moves straight to the parked list; a running one is cancelled and
// moves there once its transfer has returned; an already set-aside one just
// gets the new time (its old timer, if any, is invalidated first so it can
// never fire against the new one). done is not told: the job is still held,
// so passes leave the file alone and never count its folder as settled.
// False when the lane doesn't hold abs.
func (l *lane) park(abs string, until time.Time) bool {
	l.mu.Lock()
	j, st := l.findLocked(abs)
	switch st {
	case laneWaiting:
		l.removeQueuedLocked(j)
		j.until = until
		l.parkLocked(j)
		l.mu.Unlock()
		if j.onPark != nil {
			j.onPark()
		}
	case laneRunning:
		j.until = until
		j.parking = true
		j.cancel()
		l.mu.Unlock()
	case laneParked:
		l.invalidateTimerLocked(j)
		j.until = until
		l.armLocked(j)
		l.mu.Unlock()
	default:
		l.mu.Unlock()
		return false
	}
	l.changed()
	return true
}

// unpark takes the set-aside job at abs out of the lane; done hears
// errLaneUnparked. False when abs isn't set aside.
func (l *lane) unpark(abs string) bool {
	l.mu.Lock()
	j, st := l.findLocked(abs)
	l.mu.Unlock()
	if st != laneParked {
		return false
	}
	return l.unparkJob(j, 0)
}

// timerUnpark is what a job's set-aside timer calls when its time comes. gen
// is the generation the timer was armed with; if the job has since been
// re-armed (park() changed the time while the timer was already in flight),
// j.timerGen has moved on and this call is a no-op.
func (l *lane) timerUnpark(j *laneJob, gen uint64) bool {
	return l.unparkJob(j, gen)
}

// unparkJob is unpark for a known job; a set-aside timer calls it too via
// timerUnpark. gen is 0 for a manual/explicit unpark (always acts so long as
// the job is still set aside) or the timer's generation, in which case the
// call is a no-op if the job has been re-armed (or removed) since. Returns
// whether it actually unparked the job.
func (l *lane) unparkJob(j *laneJob, gen uint64) bool {
	l.mu.Lock()
	if gen != 0 && j.timerGen != gen {
		l.mu.Unlock()
		return false
	}
	found := false
	for i, o := range l.parked {
		if o == j {
			l.parked = append(l.parked[:i], l.parked[i+1:]...)
			found = true
			break
		}
	}
	if found {
		l.invalidateTimerLocked(j)
	}
	l.mu.Unlock()
	if !found {
		return false
	}
	j.done(errLaneUnparked)
	l.changed()
	return true
}

// laneEntry is one job as the queue view shows it.
type laneEntry struct {
	pk, rel, abs, dir string
	up                bool
	size, sent        int64
	state             laneState
	paused            bool      // waiting while the lane is paused
	pos               int       // 1-based place in the queue, for waiting jobs
	until             time.Time // set aside until; zero = until resumed
}

// entries is a snapshot of the lane: running jobs in order of arrival, then
// the queue in order, then set-aside jobs. A running job being set aside
// already shows as set aside.
func (l *lane) entries() []laneEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	mk := func(j *laneJob, st laneState) laneEntry {
		return laneEntry{pk: j.pk, rel: j.rel, abs: j.abs, dir: j.dir, up: j.up,
			size: j.size, sent: j.sent.Load(), state: st, until: j.until}
	}
	run := make([]*laneJob, 0, len(l.running))
	for j := range l.running {
		run = append(run, j)
	}
	sort.Slice(run, func(a, b int) bool { return run[a].seq < run[b].seq })
	out := make([]laneEntry, 0, len(run)+len(l.queue)+len(l.parked))
	for _, j := range run {
		st := laneRunning
		if j.parking {
			st = laneParked
		}
		out = append(out, mk(j, st))
	}
	for i, j := range l.queue {
		en := mk(j, laneWaiting)
		en.pos, en.paused = i+1, l.paused
		out = append(out, en)
	}
	for _, j := range l.parked {
		out = append(out, mk(j, laneParked))
	}
	return out
}
