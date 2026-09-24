//go:build windows

package vfs

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// pausable wires Ops.Paused to a flag the test flips.
func pausable(ops Ops, paused *atomic.Bool) Ops {
	ops.Paused = paused.Load
	return ops
}

// settled waits for the watcher's upload runs to finish their bookkeeping
// after the fake upload returns, so the test's cleanup doesn't swap the cfapi
// seams back under a run still using them.
func settled(t *testing.T, w *Watcher) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		w.mu.Lock()
		n := len(w.inflight) + len(w.upload)
		w.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the watcher never settled")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A change made while the user has paused syncing (Deck #723) must not start
// an upload: the file waits, still dirty, and goes up once the pause ends.
func TestPausedUploadIsHeldUntilResume(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	doc := filepath.Join(root, "report.docx")
	if err := os.WriteFile(doc, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDirty(doc)
	rec := newRecorder()
	var paused atomic.Bool
	paused.Store(true)
	w := bareWatcher(root, pausable(rec.ops(), &paused))
	defer w.cancel()

	w.handleChange(doc)
	select {
	case got := <-rec.uploaded:
		t.Fatalf("uploaded %q while paused", got)
	case <-time.After(300 * time.Millisecond):
	}
	if !rec.logged("sync is paused") {
		t.Errorf("no log line says the upload is waiting on the pause; logs: %v", rec.logLines())
	}
	rec.mu.Lock()
	for _, r := range rec.reports {
		if r.err != nil {
			t.Errorf("a pause was reported as a failure: %+v", r)
		}
	}
	rec.mu.Unlock()

	paused.Store(false)
	w.PauseChanged()
	select {
	case got := <-rec.uploaded:
		if got != "report.docx" {
			t.Fatalf("uploaded %q, want report.docx", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the held upload never ran after the pause ended")
	}
	settled(t, w)
}

// Pausing stops an upload that is already running (a big file takes hours),
// quietly, and it starts again when the pause ends.
func TestPauseStopsARunningUploadAndResumesIt(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	doc := filepath.Join(root, "big.bin")
	if err := os.WriteFile(doc, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDirty(doc)
	rec := newRecorder()
	gate := make(chan struct{})
	rec.uploadGate = gate
	var paused atomic.Bool
	w := bareWatcher(root, pausable(rec.ops(), &paused))
	defer w.cancel()

	go w.handleChange(doc)
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec.mu.Lock()
		n := len(rec.uploads)
		rec.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("upload never started")
		}
		time.Sleep(5 * time.Millisecond)
	}

	paused.Store(true)
	w.PauseChanged()
	deadline = time.Now().Add(5 * time.Second)
	for {
		rec.mu.Lock()
		n := len(rec.cancelled)
		rec.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pausing did not stop the running upload")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The cancelled run has to finish its bookkeeping before it counts as held.
	deadline = time.Now().Add(5 * time.Second)
	for !rec.logged("sync is paused") {
		if time.Now().After(deadline) {
			t.Fatalf("the stopped upload was not held; logs: %v", rec.logLines())
		}
		time.Sleep(5 * time.Millisecond)
	}
	rec.mu.Lock()
	for _, r := range rec.reports {
		if r.err != nil {
			t.Errorf("a pause was reported as a failure: %+v", r)
		}
	}
	rec.uploadGate = nil
	rec.mu.Unlock()
	close(gate)

	paused.Store(false)
	w.PauseChanged()
	select {
	case got := <-rec.uploaded:
		if got != "big.bin" {
			t.Fatalf("uploaded %q, want big.bin", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stopped upload never started again after the pause ended")
	}
	settled(t, w)
}

// A pause that ends between the gate's check and the upload being recorded as
// held must not strand it: nothing else would come back for it until the next
// pause change.
func TestUploadHeldAsThePauseEndsStillRuns(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	doc := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(doc, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDirty(doc)
	rec := newRecorder()
	ops := rec.ops()
	var calls atomic.Int32
	ops.Paused = func() bool { return calls.Add(1) == 1 } // paused at the gate only
	w := bareWatcher(root, ops)
	defer w.cancel()

	w.handleChange(doc) // no PauseChanged: the resume "already happened"
	select {
	case got := <-rec.uploaded:
		if got != "notes.txt" {
			t.Fatalf("uploaded %q, want notes.txt", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an upload held just as the pause ended was stranded")
	}
	settled(t, w)
}

// "Always keep on this device" downloads wait for the pause too, and none is
// lost: every pinned file comes down once it ends.
func TestPinnedHydrationWaitsForThePause(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	var paths []string
	for _, n := range []string{"a", "b", "c"} {
		p := filepath.Join(root, n+".bin")
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		f.markPinned(p)
		paths = append(paths, p)
	}
	rec := newRecorder()
	var paused atomic.Bool
	paused.Store(true)
	w := bareWatcher(root, pausable(rec.ops(), &paused))
	defer w.cancel()

	for _, p := range paths {
		w.requestHydration(p)
	}
	time.Sleep(300 * time.Millisecond)
	if got := f.hydratedPaths(); len(got) != 0 {
		t.Fatalf("downloaded %v while paused", got)
	}

	paused.Store(false)
	w.PauseChanged()
	deadline := time.Now().Add(3 * time.Second)
	for len(f.hydratedPaths()) < len(paths) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := f.hydratedPaths(); len(got) != len(paths) {
		t.Errorf("downloaded %d of %d pinned files after the pause ended: %v", len(got), len(paths), got)
	}
}

// Pause holds file data, not names: a folder made while paused still reaches
// the server, so a later move into it doesn't fail on a missing parent.
func TestPauseDoesNotHoldFolderCreates(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	dir := filepath.Join(root, "Invoices")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f.markPlain(dir) // a new folder: never uploaded
	rec := newRecorder()
	var paused atomic.Bool
	paused.Store(true)
	w := bareWatcher(root, pausable(rec.ops(), &paused))
	defer w.cancel()

	w.handleChange(dir)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.mkdirs) != 1 || rec.mkdirs[0] != "Invoices" {
		t.Errorf("mkdirs = %v, want [Invoices]", rec.mkdirs)
	}
}
