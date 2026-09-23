package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
)

// The long-transfer lane (Deck #702). A sync pass plans everything up front
// and a pair's passes run one after another, so a file saved while a 300 GB
// upload was running waited for that upload to end before it could sync.
// Transfers of laneMinBytes or more now leave the pass and run here instead:
// per engine, laneWorkers at a time, first come first served, outliving the
// pass that planned them. The pass ends in seconds and the next one can start.
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

type laneJob struct {
	pk, rel string // identity: pair key + pair-relative path
	abs     string // local path, for stops by folder
	size    int64
	run     func(ctx context.Context) error // the transfer; ctx is cancelled to stop it
	done    func(err error)                 // told once, when the job leaves the lane for good

	// Set while running; guarded by lane.mu.
	cancel  context.CancelFunc
	ended   chan struct{} // closed once a running attempt has returned and done (if due) ran
	requeue bool          // cancelled by pause: back to the queue, done not told
	stopped bool          // cancelled by stop or close: done hears errLaneStopped
}

type lane struct {
	mu      sync.Mutex
	queue   []*laneJob
	running map[*laneJob]bool
	paused  bool
	closed  bool
	wg      sync.WaitGroup
}

func newLane(paused bool) *lane {
	return &lane{running: make(map[*laneJob]bool), paused: paused}
}

// relUnder reports whether the pair-relative path p is dir or lies beneath it.
// "" is the pair's root, so everything is under it.
func relUnder(p, dir string) bool {
	return dir == "" || p == dir || strings.HasPrefix(p, dir+"/")
}

// add queues j. False when the lane is closed or already holds j's path.
func (l *lane) add(j *laneJob) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.holdsLocked(func(o *laneJob) bool { return o.pk == j.pk && o.rel == j.rel }) {
		return false
	}
	l.queue = append(l.queue, j)
	l.dispatchLocked()
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
	return false
}

// has reports a queued or running job for exactly rel in pair pk.
func (l *lane) has(pk, rel string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.holdsLocked(func(j *laneJob) bool { return j.pk == pk && j.rel == rel })
}

// covers reports a queued or running job for rel or anything beneath it.
func (l *lane) covers(pk, rel string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.holdsLocked(func(j *laneJob) bool { return j.pk == pk && relUnder(j.rel, rel) })
}

// count is how many jobs are queued or running.
func (l *lane) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.queue) + len(l.running)
}

func (l *lane) dispatchLocked() {
	for !l.paused && !l.closed && len(l.running) < laneWorkers && len(l.queue) > 0 {
		j := l.queue[0]
		l.queue = l.queue[1:]
		ctx, cancel := context.WithCancel(context.Background())
		j.cancel, j.ended, j.requeue, j.stopped = cancel, make(chan struct{}), false, false
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
	finished := true
	if err != nil && j.requeue && !l.closed {
		// Paused mid-transfer: back to the front, to carry on when unpaused.
		// Chunk resume and .nimbo-part mean it picks up, not starts over.
		l.queue = append([]*laneJob{j}, l.queue...)
		finished = false
	}
	if err != nil && j.stopped {
		err = errLaneStopped
	}
	l.dispatchLocked()
	l.mu.Unlock()

	if finished {
		j.done(err)
	}
	close(ended)
}

// pause cancels the running jobs and holds the queue until resume. The
// cancelled jobs go back to the front of the queue; done is not told.
func (l *lane) pause() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.paused {
		return
	}
	l.paused = true
	for j := range l.running {
		j.requeue = true
		j.cancel()
	}
}

// resume lets the queue run again.
func (l *lane) resume() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.paused = false
	l.dispatchLocked()
}

// stop takes every job match accepts out of the lane: queued ones at once,
// running ones cancelled. It returns once all of them have ended and been
// told (errLaneStopped, or the transfer's own result if it finished first),
// so the caller can move or delete their files.
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
	var waits []chan struct{}
	for j := range l.running {
		if match(j) {
			j.stopped, j.requeue = true, false
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
