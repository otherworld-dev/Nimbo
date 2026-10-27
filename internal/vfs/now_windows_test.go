//go:build windows

package vfs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/otherworld/nimbo/internal/cfapi"
)

func waitListed(t *testing.T, rec *recorder, rel string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		rec.mu.Lock()
		n := rec.listCalls[rel]
		rec.mu.Unlock()
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%q never listed", rel)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWatcherNowCountsFoldersDuringAPass(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	rec := newRecorder()
	gate := make(chan struct{})
	rec.listGate = gate
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	done := make(chan struct{})
	go func() { w.Reconcile(); close(done) }()
	waitListed(t, rec, "")
	if n := w.Now(); !n.Checking {
		t.Errorf("Checking = false while the pass is held: %+v", n)
	}
	close(gate)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("pass never finished")
	}
	n := w.Now()
	if n.Checking {
		t.Errorf("Checking = true after the pass: %+v", n)
	}
	if n.DirsChecked != 1 {
		t.Errorf("DirsChecked = %d, want 1 (the root)", n.DirsChecked)
	}
}

func TestWatcherNowLastFullCheckOnlyOnACompletePass(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bad"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bad", "x.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	rec := newRecorder()
	rec.listing[""] = []cfapi.PlaceholderInfo{ph("bad", true, "e-bad", "")}
	rec.listing["bad"] = []cfapi.PlaceholderInfo{ph("x.txt", false, "e-x", "f-x")}
	rec.listErrs["bad"] = errors.New("503")
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.Reconcile()
	n := w.Now()
	if !n.CheckIncomplete || !n.LastFullCheck.IsZero() {
		t.Errorf("after a pass with a failing folder: %+v, want CheckIncomplete and no LastFullCheck", n)
	}
	rec.mu.Lock()
	delete(rec.listErrs, "bad")
	rec.mu.Unlock()
	w.Reconcile()
	n = w.Now()
	if n.CheckIncomplete || n.LastFullCheck.IsZero() {
		t.Errorf("after a complete pass: %+v, want a LastFullCheck and no CheckIncomplete", n)
	}
}

func TestWatcherNowListsAnUploadInFlight(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	doc := filepath.Join(root, "doc.txt")
	if err := os.WriteFile(doc, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDirty(doc)
	rec := newRecorder()
	gate := make(chan struct{})
	rec.uploadGate = gate
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	done := make(chan struct{})
	go func() { w.handleChange(doc); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if up := w.Now().Uploading; len(up) == 1 && up[0] == doc {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Uploading = %v, want [%s]", w.Now().Uploading, doc)
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(gate)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("upload never finished")
	}
	if up := w.Now().Uploading; len(up) != 0 {
		t.Errorf("Uploading = %v after the upload, want empty", up)
	}
}

func TestWatcherChangedHookFires(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	rec := newRecorder()
	var calls atomic.Int32
	ops := rec.ops()
	ops.Changed = func() { calls.Add(1) }
	w := bareWatcher(root, ops)
	defer w.cancel()

	w.Reconcile()
	if n := calls.Load(); n < 2 {
		t.Errorf("Changed called %d times for one pass, want at least 2 (start and end)", n)
	}
}

// A pass says when it started, so the GUI can keep routine sub-second passes
// off the status line (Deck #988 review); between passes it is zero.
func TestWatcherNowCheckingSince(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	rec := newRecorder()
	gate := make(chan struct{})
	rec.listGate = gate
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	if n := w.Now(); !n.CheckingSince.IsZero() {
		t.Errorf("CheckingSince = %v before any pass, want zero", n.CheckingSince)
	}
	before := time.Now()
	done := make(chan struct{})
	go func() { w.Reconcile(); close(done) }()
	waitListed(t, rec, "")
	n := w.Now()
	if !n.Checking || n.CheckingSince.Before(before) || n.CheckingSince.After(time.Now()) {
		t.Errorf("during the pass: Checking=%v CheckingSince=%v, want it set at the pass start (after %v)", n.Checking, n.CheckingSince, before)
	}
	close(gate)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("pass never finished")
	}
	if n := w.Now(); !n.CheckingSince.IsZero() {
		t.Errorf("CheckingSince = %v after the pass, want zero", n.CheckingSince)
	}
}

// Uploads queued (debounced, backed off) or held by a pause count as pending,
// each path once (Deck #988 review: on-demand read "Up to date" with changes
// still to go up).
func TestWatcherNowCountsPendingUploads(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	w := bareWatcher(root, newRecorder().ops())
	defer w.cancel()

	a := filepath.Join(root, "a.txt")
	b := filepath.Join(root, "B.txt")
	w.scheduleUploadAfter(a, time.Hour)
	w.scheduleUploadAfter(b, time.Hour)
	w.mu.Lock()
	w.held = map[string]string{
		strings.ToLower(b):                        b, // queued and held: once
		strings.ToLower(filepath.Join(root, "c")): filepath.Join(root, "c"),
	}
	w.mu.Unlock()
	if n := w.Now(); n.Pending != 3 {
		t.Errorf("Pending = %d, want 3", n.Pending)
	}
	w.mu.Lock()
	for _, tm := range w.upload {
		tm.Stop()
	}
	w.upload = map[string]*time.Timer{}
	w.held = nil
	w.mu.Unlock()
	if n := w.Now(); n.Pending != 0 {
		t.Errorf("Pending = %d with nothing queued, want 0", n.Pending)
	}
}

// A pinned download that is backed off, or no longer pinned, never lists as
// a download in progress, not even for a moment.
func TestHydratePinnedSkipsWithoutAPhantomDownload(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	rec := newRecorder()
	var calls atomic.Int32
	ops := rec.ops()
	ops.Changed = func() { calls.Add(1) }
	w := bareWatcher(root, ops)
	defer w.cancel()

	notPinned := filepath.Join(root, "plain.txt")
	w.hydratePinned(notPinned)
	if n := calls.Load(); n != 0 {
		t.Errorf("not pinned: Changed called %d times, want 0 (no Hydrating entry)", n)
	}

	backed := filepath.Join(root, "backed.txt")
	f.mu.Lock()
	f.pinned[strings.ToLower(backed)] = true
	f.mu.Unlock()
	w.mu.Lock()
	w.hydrateNext[strings.ToLower(backed)] = time.Now().Add(time.Hour)
	w.mu.Unlock()
	w.hydratePinned(backed)
	if n := calls.Load(); n != 0 {
		t.Errorf("backed off: Changed called %d times, want 0", n)
	}

	w.mu.Lock()
	delete(w.hydrateNext, strings.ToLower(backed))
	w.mu.Unlock()
	w.hydratePinned(backed)
	if n := calls.Load(); n < 2 {
		t.Errorf("pinned: Changed called %d times, want start and end", n)
	}
}
