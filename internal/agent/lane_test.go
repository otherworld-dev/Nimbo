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
	doneErr chan error // what done was told; buffered, done is called once
}

func newTestJob(pk, rel string) *testJob {
	tj := &testJob{release: make(chan struct{}), doneErr: make(chan error, 1)}
	tj.j = &laneJob{pk: pk, rel: rel, abs: filepath.Join(`C:\Sync`, filepath.FromSlash(rel))}
	tj.j.run = func(ctx context.Context) error {
		tj.starts.Add(1)
		select {
		case <-tj.release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	tj.j.done = func(err error) { tj.doneErr <- err }
	return tj
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
