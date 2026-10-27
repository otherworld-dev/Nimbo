//go:build windows

package vfs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Deck #991: an upload that failed while the server was unreachable waited out
// its backoff (up to 30 minutes) after the network came back, because nothing
// told it the outage was over. The first successful root listing after failed
// ones brings every backed-off retry forward.
func TestReconcileAfterAnOutageRetriesBackedOffUploadsNow(t *testing.T) {
	f := installFakeCf(t)
	oldBase, oldMax, oldNow := retryBase, retryMax, retryNowWindow
	retryBase, retryMax, retryNowWindow = time.Hour, time.Hour, 50*time.Millisecond
	t.Cleanup(func() { retryBase, retryMax, retryNowWindow = oldBase, oldMax, oldNow })

	root := t.TempDir()
	doc := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(doc, []byte("edited offline"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.markDirty(doc)
	f.markModified(doc)

	rec := newRecorder()
	rec.uploadFails = 1
	rec.uploadErr = errors.New("dial tcp: lookup server: no such host")
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	// Offline: the upload fails and backs off for an hour; reconcile can't list.
	w.handleChange(doc)
	rec.mu.Lock()
	left := rec.uploadFails
	rec.listErr = errors.New("dial tcp: lookup server: no such host")
	rec.mu.Unlock()
	if left != 0 {
		t.Fatal("the offline upload attempt never ran")
	}
	w.mu.Lock()
	_, armed := w.upload[doc]
	w.mu.Unlock()
	if !armed {
		t.Fatal("the failed upload was not armed for a retry")
	}
	w.Reconcile()

	// Back online: the root lists again.
	rec.mu.Lock()
	rec.listErr = nil
	rec.mu.Unlock()
	w.Reconcile()

	select {
	case got := <-rec.uploaded:
		if !strings.EqualFold(got, "notes.txt") {
			t.Fatalf("uploaded %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the backed-off upload still waits for its hour-long backoff after the outage ended")
	}
}

// A retry that failed for a reason other than the outage still backs off: only
// one early attempt is made per recovery, and its backoff count is kept.
func TestRetryNowKeepsTheBackoffCount(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()
	p := filepath.Join(root, "x.txt")
	w.mu.Lock()
	w.attempts[strings.ToLower(p)] = 5
	w.mu.Unlock()
	w.scheduleUploadAfter(p, time.Hour)

	w.retryNow()

	w.mu.Lock()
	n := w.attempts[strings.ToLower(p)]
	w.mu.Unlock()
	if n != 5 {
		t.Errorf("attempts = %d after retryNow, want 5 (kept)", n)
	}
}
