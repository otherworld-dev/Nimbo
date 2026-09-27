package agent

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// waitFor polls cond until it holds, failing the test after 5 seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// returnsSoon runs fn and fails the test if it has not returned within 5
// seconds: something waited for a large transfer it should have left alone.
// fn must not call t.Fatal (it runs on another goroutine); pass results out.
func returnsSoon(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not return: it waited for a large transfer", what)
	}
}

// testJob is a lane job whose transfer runs until released or cancelled.
type testJob struct {
	j       *laneJob
	release chan struct{}
	starts  atomic.Int32
	parks   atomic.Int32 // times onPark ran
	dones   atomic.Int32 // times done ran; must never pass 1
	doneErr chan error   // what done was told first
}

func newTestJob(pk, rel string) *testJob {
	tj := &testJob{release: make(chan struct{}), doneErr: make(chan error, 1)}
	tj.j = &laneJob{pk: pk, rel: rel, dir: `C:\Sync`, abs: filepath.Join(`C:\Sync`, filepath.FromSlash(rel)), size: 100}
	tj.j.run = func(ctx context.Context) error {
		tj.starts.Add(1)
		select {
		case <-tj.release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	tj.j.onPark = func() { tj.parks.Add(1) }
	tj.j.done = func(err error) {
		if tj.dones.Add(1) == 1 {
			tj.doneErr <- err
		}
	}
	return tj
}

// noDone fails the test if done has been told anything.
func noDone(t *testing.T, tj *testJob, what string) {
	t.Helper()
	if n := tj.dones.Load(); n != 0 {
		t.Fatalf("%s: done was told %d time(s)", what, n)
	}
}

func TestLaneRunsTwoAtATimeInOrder(t *testing.T) {
	l := newLane(false)
	t.Cleanup(l.close)
	jobs := []*testJob{newTestJob("pk", "a"), newTestJob("pk", "b"), newTestJob("pk", "c"), newTestJob("pk", "d")}
	for _, tj := range jobs {
		if !l.add(tj.j) {
			t.Fatalf("lane refused %s", tj.j.rel)
		}
	}
	waitFor(t, "the first two to start", func() bool { return jobs[0].starts.Load() == 1 && jobs[1].starts.Load() == 1 })
	time.Sleep(50 * time.Millisecond)
	if jobs[2].starts.Load() != 0 || jobs[3].starts.Load() != 0 {
		t.Fatal("more than two large transfers ran at once")
	}
	close(jobs[0].release)
	if err := <-jobs[0].doneErr; err != nil {
		t.Fatalf("finished job reported %v", err)
	}
	waitFor(t, "the third to start", func() bool { return jobs[2].starts.Load() == 1 })
	if jobs[3].starts.Load() != 0 {
		t.Fatal("the fourth jumped the queue")
	}
}

func TestLaneHoldsAPathOnce(t *testing.T) {
	l := newLane(true) // paused: jobs stay queued, nothing runs
	t.Cleanup(l.close)
	if !l.add(newTestJob("pk", "D/big.bin").j) {
		t.Fatal("lane refused a new file")
	}
	if l.add(newTestJob("pk", "D/big.bin").j) {
		t.Fatal("lane took the same file twice")
	}
	if !l.add(newTestJob("other", "D/big.bin").j) {
		t.Fatal("lane refused the same path in another folder pair")
	}
	for _, c := range []struct {
		rel         string
		has, covers bool
	}{
		{"D/big.bin", true, true},
		{"D", false, true},
		{"", false, true},
		{"DD", false, false},
		{"D/big", false, false},
		{"D/big.bin/x", false, false},
	} {
		if got := l.has("pk", c.rel); got != c.has {
			t.Errorf("has(%q) = %v, want %v", c.rel, got, c.has)
		}
		if got := l.covers("pk", c.rel); got != c.covers {
			t.Errorf("covers(%q) = %v, want %v", c.rel, got, c.covers)
		}
	}
	if n := l.count(); n != 2 {
		t.Errorf("count = %d, want 2", n)
	}
}

func TestLanePauseStopsRunningJobsAndResumeRestartsThem(t *testing.T) {
	l := newLane(false)
	t.Cleanup(l.close)
	a := newTestJob("pk", "a")
	l.add(a.j)
	waitFor(t, "a to start", func() bool { return a.starts.Load() == 1 })

	l.pause()
	waitFor(t, "the running job to go back to the queue", func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		return len(l.running) == 0 && len(l.queue) == 1
	})
	select {
	case err := <-a.doneErr:
		t.Fatalf("a paused job was reported done: %v", err)
	default:
	}
	b := newTestJob("pk", "b")
	l.add(b.j)
	time.Sleep(50 * time.Millisecond)
	if a.starts.Load() != 1 || b.starts.Load() != 0 {
		t.Fatal("a job started while the lane was paused")
	}

	close(a.release)
	l.resume()
	if err := <-a.doneErr; err != nil {
		t.Fatalf("resumed job reported %v", err)
	}
	if n := a.starts.Load(); n != 2 {
		t.Errorf("paused job ran %d times, want 2 (once, then again on resume)", n)
	}
}

func TestLaneStopWaitsForTheJobsItStops(t *testing.T) {
	l := newLane(false)
	t.Cleanup(l.close)
	running := newTestJob("pk", "D/a")
	other := newTestJob("pk", "E/b")
	queued := newTestJob("pk", "D/c")
	l.add(running.j)
	l.add(other.j)
	l.add(queued.j)
	waitFor(t, "two to start", func() bool { return running.starts.Load() == 1 && other.starts.Load() == 1 })

	l.stop(func(j *laneJob) bool { return j.pk == "pk" && relUnder(j.rel, "D") })

	for name, tj := range map[string]*testJob{"running": running, "queued": queued} {
		select {
		case err := <-tj.doneErr:
			if !errors.Is(err, errLaneStopped) {
				t.Errorf("%s job: done got %v, want errLaneStopped", name, err)
			}
		default:
			t.Fatalf("stop returned before the %s job was done", name)
		}
	}
	if queued.starts.Load() != 0 {
		t.Error("a stopped queued job ran")
	}
	if n := l.count(); n != 1 {
		t.Errorf("count = %d after the stop, want 1 (the job outside D)", n)
	}
}

func TestLaneCloseStopsEverythingAndRefusesMore(t *testing.T) {
	l := newLane(false)
	a := newTestJob("pk", "a")
	l.add(a.j)
	waitFor(t, "a to start", func() bool { return a.starts.Load() == 1 })
	l.close()
	select {
	case err := <-a.doneErr:
		if !errors.Is(err, errLaneStopped) {
			t.Errorf("done got %v, want errLaneStopped", err)
		}
	default:
		t.Fatal("close returned before the running job ended")
	}
	if l.add(newTestJob("pk", "b").j) {
		t.Fatal("a closed lane took a job")
	}
}

func TestLaneSyncFirstMovesAWaitingJobToTheFront(t *testing.T) {
	l := newLane(false)
	t.Cleanup(l.close)
	a, b, c, d := newTestJob("pk", "a"), newTestJob("pk", "b"), newTestJob("pk", "c"), newTestJob("pk", "d")
	for _, tj := range []*testJob{a, b, c, d} {
		l.add(tj.j)
	}
	waitFor(t, "a and b to start", func() bool { return a.starts.Load() == 1 && b.starts.Load() == 1 })

	if !l.first(d.j.abs) {
		t.Fatal("first refused a waiting job")
	}
	if l.first(a.j.abs) {
		t.Fatal("first accepted a running job")
	}
	if l.first(`C:\Sync\nope`) {
		t.Fatal("first accepted a path the lane doesn't hold")
	}
	close(a.release)
	waitFor(t, "d to start", func() bool { return d.starts.Load() == 1 })
	if c.starts.Load() != 0 {
		t.Fatal("c started ahead of d, which was moved to the front")
	}
	if a.starts.Load() != 1 || b.starts.Load() != 1 {
		t.Fatal("sync first restarted a running job")
	}
}

func TestLaneSetAsideAWaitingJob(t *testing.T) {
	l := newLane(true) // paused: a stays queued
	t.Cleanup(l.close)
	a := newTestJob("pk", "D/a")
	l.add(a.j)
	if !l.park(a.j.abs, time.Time{}) {
		t.Fatal("park refused a waiting job")
	}
	if a.parks.Load() != 1 {
		t.Fatalf("onPark ran %d times, want 1", a.parks.Load())
	}
	noDone(t, a, "set aside")
	if l.count() != 0 || l.parkedCount() != 1 {
		t.Fatalf("count=%d parked=%d, want 0 and 1", l.count(), l.parkedCount())
	}
	if !l.has("pk", "D/a") || !l.covers("pk", "D") {
		t.Fatal("a set-aside job is no longer held: passes would transfer it")
	}
	if l.add(newTestJob("pk", "D/a").j) {
		t.Fatal("the lane took a second job for a set-aside path")
	}
	l.resume()
	time.Sleep(50 * time.Millisecond)
	if a.starts.Load() != 0 {
		t.Fatal("a set-aside job started on resume")
	}
}

func TestLaneSetAsideARunningJobFreesItsWorker(t *testing.T) {
	l := newLane(false)
	t.Cleanup(l.close)
	a, b, c := newTestJob("pk", "a"), newTestJob("pk", "b"), newTestJob("pk", "c")
	for _, tj := range []*testJob{a, b, c} {
		l.add(tj.j)
	}
	waitFor(t, "a and b to start", func() bool { return a.starts.Load() == 1 && b.starts.Load() == 1 })

	if !l.park(a.j.abs, time.Time{}) {
		t.Fatal("park refused a running job")
	}
	waitFor(t, "c to take a's worker", func() bool { return c.starts.Load() == 1 })
	waitFor(t, "onPark", func() bool { return a.parks.Load() == 1 })
	noDone(t, a, "set aside mid-run")
	if l.parkedCount() != 1 {
		t.Fatalf("parked = %d, want 1", l.parkedCount())
	}
}

func TestLaneResumeASetAsideJob(t *testing.T) {
	l := newLane(true)
	t.Cleanup(l.close)
	a := newTestJob("pk", "a")
	l.add(a.j)
	if l.unpark(a.j.abs) {
		t.Fatal("unpark accepted a job that isn't set aside")
	}
	l.park(a.j.abs, time.Time{})
	if !l.unpark(a.j.abs) {
		t.Fatal("unpark refused a set-aside job")
	}
	if err := <-a.doneErr; !errors.Is(err, errLaneUnparked) {
		t.Fatalf("done got %v, want errLaneUnparked", err)
	}
	if l.has("pk", "a") {
		t.Fatal("the lane still holds a job that came back from set-aside")
	}
	if l.unpark(a.j.abs) {
		t.Fatal("unpark accepted the same job twice")
	}
}

func TestLaneSetAsideTimeRunsOut(t *testing.T) {
	l := newLane(true) // also covers a timer running out while paused
	t.Cleanup(l.close)
	a := newTestJob("pk", "a")
	l.add(a.j)
	l.park(a.j.abs, time.Now().Add(50*time.Millisecond))
	select {
	case err := <-a.doneErr:
		if !errors.Is(err, errLaneUnparked) {
			t.Fatalf("done got %v, want errLaneUnparked", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the set-aside time ran out and nothing happened")
	}
	if l.parkedCount() != 0 {
		t.Fatal("still set aside after its time ran out")
	}
}

func TestLaneSetAsideAgainChangesTheTime(t *testing.T) {
	l := newLane(true)
	t.Cleanup(l.close)
	a := newTestJob("pk", "a")
	l.add(a.j)
	l.park(a.j.abs, time.Now().Add(50*time.Millisecond))
	if !l.park(a.j.abs, time.Time{}) { // now: until I resume it
		t.Fatal("park refused an already set-aside job")
	}
	time.Sleep(150 * time.Millisecond)
	noDone(t, a, "the old timer")
	if a.parks.Load() != 1 {
		t.Fatalf("onPark ran %d times, want 1 (changing the time is not a second park)", a.parks.Load())
	}
}

func TestLaneStopDropsASetAsideJobAndItsTimer(t *testing.T) {
	l := newLane(true)
	t.Cleanup(l.close)
	a := newTestJob("pk", "D/a")
	l.add(a.j)
	l.park(a.j.abs, time.Now().Add(100*time.Millisecond))
	l.stop(func(j *laneJob) bool { return relUnder(j.rel, "D") })
	if err := <-a.doneErr; !errors.Is(err, errLaneStopped) {
		t.Fatalf("done got %v, want errLaneStopped", err)
	}
	time.Sleep(250 * time.Millisecond)
	if n := a.dones.Load(); n != 1 {
		t.Fatalf("done was told %d times: the timer fired after the stop", n)
	}
	if l.has("pk", "D/a") {
		t.Fatal("the lane still holds a stopped set-aside job")
	}
}

func TestLanePauseLeavesSetAsideJobsAlone(t *testing.T) {
	l := newLane(false)
	t.Cleanup(l.close)
	a := newTestJob("pk", "a")
	l.add(a.j)
	waitFor(t, "a to start", func() bool { return a.starts.Load() == 1 })
	l.park(a.j.abs, time.Time{})
	waitFor(t, "a to be set aside", func() bool { return l.parkedCount() == 1 })
	l.pause()
	l.resume()
	time.Sleep(50 * time.Millisecond)
	if a.starts.Load() != 1 || l.parkedCount() != 1 {
		t.Fatal("pause and resume brought a set-aside job back")
	}
	b := newTestJob("pk", "b")
	l.pause()
	l.add(b.j)
	if !l.park(b.j.abs, time.Time{}) {
		t.Fatal("park refused a job while the lane was paused")
	}
}

func TestLaneEntriesListRunningThenWaitingThenSetAside(t *testing.T) {
	l := newLane(false)
	t.Cleanup(l.close)
	var changes atomic.Int32
	l.onChange = func() { changes.Add(1) }
	a, b, c, d, e := newTestJob("pk", "a"), newTestJob("pk", "b"), newTestJob("pk", "c"), newTestJob("pk", "d"), newTestJob("pk", "e")
	a.j.up = true
	for _, tj := range []*testJob{a, b, c, d, e} {
		l.add(tj.j)
	}
	waitFor(t, "a and b to start", func() bool { return a.starts.Load() == 1 && b.starts.Load() == 1 })
	until := time.Now().Add(time.Hour)
	l.park(e.j.abs, until)

	got := l.entries()
	want := []struct {
		rel   string
		state laneState
		pos   int
	}{{"a", laneRunning, 0}, {"b", laneRunning, 0}, {"c", laneWaiting, 1}, {"d", laneWaiting, 2}, {"e", laneParked, 0}}
	if len(got) != len(want) {
		t.Fatalf("%d entries, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].rel != w.rel || got[i].state != w.state || got[i].pos != w.pos {
			t.Errorf("entry %d = %s/%v/%d, want %s/%v/%d", i, got[i].rel, got[i].state, got[i].pos, w.rel, w.state, w.pos)
		}
	}
	if !got[0].up || got[1].up {
		t.Error("upload flag not carried")
	}
	if got[0].dir != `C:\Sync` {
		t.Errorf("dir = %q", got[0].dir)
	}
	if !got[4].until.Equal(until) {
		t.Errorf("until = %v, want %v", got[4].until, until)
	}
	if changes.Load() == 0 {
		t.Error("onChange never ran")
	}
	l.pause()
	for _, en := range l.entries() {
		if en.state == laneWaiting && !en.paused {
			t.Errorf("%s: waiting entry not marked paused while the lane is paused", en.rel)
		}
	}
}

// TestLaneStaleSetAsideTimerIsANoOp is a deterministic reproduction of the
// race TestLaneSetAsideAgainChangesTheTime can only probe probabilistically:
// a set-aside timer callback that reaches l.mu only after the job has been
// re-armed with a new time (park() called again while the old timer was
// already in flight) must do nothing, even though it still finds the job on
// the parked list by identity. Without the timer-generation guard, this
// callback would wrongly unpark the job and undo the user's new choice.
func TestLaneStaleSetAsideTimerIsANoOp(t *testing.T) {
	l := newLane(true)
	t.Cleanup(l.close)
	a := newTestJob("pk", "a")
	l.add(a.j)

	l.park(a.j.abs, time.Now().Add(time.Hour)) // arms generation 1; won't fire during the test
	staleGen := a.j.timerGen

	l.park(a.j.abs, time.Now().Add(time.Hour)) // re-parks: must invalidate generation 1
	if a.j.timerGen == staleGen {
		t.Fatal("re-parking an already set-aside job did not change its timer generation")
	}

	// Simulate generation 1's callback finally acquiring l.mu after the
	// re-park above already ran.
	if l.timerUnpark(a.j, staleGen) {
		t.Fatal("a stale timer callback unparked the job anyway")
	}
	noDone(t, a, "stale timer callback")
	if l.parkedCount() != 1 {
		t.Fatalf("parked = %d, want 1 (still set aside)", l.parkedCount())
	}

	// The live generation's callback must still work.
	if !l.timerUnpark(a.j, a.j.timerGen) {
		t.Fatal("the current generation's timer callback was refused")
	}
	if err := <-a.doneErr; !errors.Is(err, errLaneUnparked) {
		t.Fatalf("done got %v, want errLaneUnparked", err)
	}
}
