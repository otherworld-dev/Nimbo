//go:build windows

package vfs

// Explorer's "Sync this first" in an on-demand folder (Deck #702, stage 3).

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func mustWrite(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func wantUpload(t *testing.T, rec *recorder, want string) {
	t.Helper()
	select {
	case got := <-rec.uploaded:
		if got != want {
			t.Fatalf("uploaded %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s was never uploaded", want)
	}
}

func noUpload(t *testing.T, rec *recorder) {
	t.Helper()
	select {
	case got := <-rec.uploaded:
		t.Fatalf("uploaded %q", got)
	case <-time.After(300 * time.Millisecond):
	}
}

func hydratedHas(f *fakeCf, path string) bool {
	for _, p := range f.hydratedPaths() {
		if strings.EqualFold(p, path) {
			return true
		}
	}
	return false
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// An upload that failed waits out a backoff of up to half an hour. Asked for
// by name, it goes again now.
func TestSyncFirstRetriesAFailedUploadNow(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	doc := filepath.Join(root, "report.docx")
	mustWrite(t, doc)
	f.markDirty(doc)
	rec := newRecorder()
	rec.uploadFails, rec.uploadErr = 1, errors.New("server said 503")
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.handleChange(doc) // fails, and backs off for retryBase (30 s)
	noUpload(t, rec)

	// The menu need not spell the path the way the change events did.
	res := w.SyncFirst(strings.ToUpper(doc))
	if res.Uploads != 1 || res.Paused || res.NotSynced {
		t.Fatalf("got %+v, want one upload started", res)
	}
	wantUpload(t, rec, "report.docx")
	settled(t, w)
}

// A changed file nothing is on its way to send (its event was lost) is sent.
func TestSyncFirstUploadsAChangedFileNothingIsWaitingOn(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	doc := filepath.Join(root, "notes.txt")
	mustWrite(t, doc)
	f.markDirty(doc)
	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	if res := w.SyncFirst(doc); res.Uploads != 1 {
		t.Fatalf("got %+v, want one upload started", res)
	}
	wantUpload(t, rec, "notes.txt")
	settled(t, w)
}

func TestSyncFirstLeavesARunningUploadAlone(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	doc := filepath.Join(root, "big.bin")
	mustWrite(t, doc)
	f.markDirty(doc)
	rec := newRecorder()
	gate := make(chan struct{})
	rec.uploadGate = gate
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	go w.handleChange(doc)
	waitUntil(t, "the upload to start", func() bool {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return len(rec.uploads) == 1
	})
	if res := w.SyncFirst(doc); !res.Busy || res.Uploads != 0 {
		t.Fatalf("got %+v, want only Busy", res)
	}
	close(gate)
	wantUpload(t, rec, "big.bin")
	settled(t, w)
	rec.mu.Lock()
	n := len(rec.uploads)
	rec.mu.Unlock()
	if n != 1 {
		t.Fatalf("the file was uploaded %d times, want once", n)
	}
}

func TestSyncFirstDoesNothingWhilePaused(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	doc := filepath.Join(root, "notes.txt")
	kept := filepath.Join(root, "kept.bin")
	mustWrite(t, doc)
	mustWrite(t, kept)
	f.markDirty(doc)
	f.markPinned(kept)
	rec := newRecorder()
	var paused atomic.Bool
	paused.Store(true)
	w := bareWatcher(root, pausable(rec.ops(), &paused))
	defer w.cancel()

	for _, p := range []string{doc, kept, root} {
		if res := w.SyncFirst(p); !res.Paused || res.Uploads != 0 || res.Downloads != 0 {
			t.Fatalf("%s: got %+v, want only Paused", p, res)
		}
	}
	noUpload(t, rec)
	if n := len(f.hydratedPaths()); n != 0 {
		t.Fatalf("%d file(s) downloaded while paused", n)
	}
}

// A file kept on this device, waiting behind two large downloads, starts at
// once: the workers are not interrupted, it gets one of its own.
func TestSyncFirstDownloadsAKeptFileAheadOfTheQueue(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	var paths []string
	for _, n := range []string{"a", "b", "c", "d"} {
		p := filepath.Join(root, n+".bin")
		mustWrite(t, p)
		f.markPinned(p)
		paths = append(paths, p)
	}
	f.mu.Lock()
	f.hydrateGate = make(chan struct{})
	f.mu.Unlock()
	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	for _, p := range paths {
		w.requestHydration(p)
	}
	waitUntil(t, "a and b to start", func() bool { return hydratedHas(f, paths[0]) && hydratedHas(f, paths[1]) })

	res := w.SyncFirst(paths[3])
	if res.Downloads != 1 || res.Busy {
		t.Fatalf("got %+v, want one download moved up", res)
	}
	waitUntil(t, "d to start", func() bool { return hydratedHas(f, paths[3]) })
	if hydratedHas(f, paths[2]) {
		t.Fatal("c started, though it was behind the file that was asked for")
	}
	if got := w.SyncFirst(paths[0]); !got.Busy || got.Downloads != 0 {
		t.Fatalf("a file already downloading: got %+v, want only Busy", got)
	}

	f.mu.Lock()
	close(f.hydrateGate)
	f.mu.Unlock()
	waitUntil(t, "all four to download", func() bool { return f.hydrateCompleted() == 4 })
	for _, p := range paths {
		if n := f.hydrateCallCount(p); n != 1 {
			t.Errorf("%s was downloaded %d times, want once", filepath.Base(p), n)
		}
	}
}

// A kept file whose download failed is backing off. Asked for by name, it is
// tried again now.
func TestSyncFirstRetriesAFailedDownloadNow(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	p := filepath.Join(root, "kept.bin")
	mustWrite(t, p)
	f.markPinned(p)
	f.markHydrateErr(p, errors.New("server said 503"))
	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	w.requestHydration(p)
	waitUntil(t, "the first attempt to fail", func() bool { return f.hydrateCallCount(p) == 1 })
	waitUntil(t, "the failed attempt to end", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return !w.hydrating[strings.ToLower(p)]
	})
	w.requestHydration(p) // refused: backing off
	time.Sleep(50 * time.Millisecond)
	if n := f.hydrateCallCount(p); n != 1 {
		t.Fatalf("a download in backoff was retried (%d attempts)", n)
	}

	f.mu.Lock()
	delete(f.hydrateErr, strings.ToLower(p))
	f.mu.Unlock()
	if res := w.SyncFirst(p); res.Downloads != 1 {
		t.Fatalf("got %+v, want the download asked for", res)
	}
	waitUntil(t, "the download", func() bool { return hydratedHas(f, p) })
}

// An online-only file the user has not asked to keep on this device is in
// step with the server. It is not downloaded.
func TestSyncFirstDoesNotDownloadAnOnlineOnlyFile(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	p := filepath.Join(root, "cloud.bin")
	mustWrite(t, p)
	f.markDehydrated(p)
	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	res := w.SyncFirst(p)
	if !res.OnlineOnly || res.Downloads != 0 || res.Uploads != 0 {
		t.Fatalf("got %+v, want only OnlineOnly", res)
	}
	time.Sleep(100 * time.Millisecond)
	if n := f.hydrateCallCount(p); n != 0 {
		t.Fatalf("an online-only file was downloaded (%d attempts)", n)
	}
	noUpload(t, rec)
}

// A folder: the uploads waiting beneath it go now, its kept files' downloads
// move ahead of the others, and nothing else in it is downloaded.
func TestSyncFirstOnAFolder(t *testing.T) {
	f := installFakeCf(t)
	root := t.TempDir()
	inDoc := filepath.Join(root, "Work", "report.docx")
	outDoc := filepath.Join(root, "Other", "notes.txt")
	sibling := filepath.Join(root, "Workshop", "plan.txt") // a name that only starts like the folder's
	cloud := filepath.Join(root, "Work", "cloud.bin")
	for _, p := range []string{inDoc, outDoc, sibling, cloud} {
		mustWrite(t, p)
	}
	for _, p := range []string{inDoc, outDoc, sibling} {
		f.markDirty(p)
	}
	f.markDehydrated(cloud)
	var kept []string
	for _, rel := range []string{"Other/k1.bin", "Other/k2.bin", "Other/k3.bin", "Work/k4.bin", "Work/Deep/k5.bin"} {
		p := filepath.Join(root, filepath.FromSlash(rel))
		mustWrite(t, p)
		f.markPinned(p)
		kept = append(kept, p)
	}
	f.mu.Lock()
	f.hydrateGate = make(chan struct{})
	f.mu.Unlock()
	rec := newRecorder()
	rec.uploadFails, rec.uploadErr = 3, errors.New("server said 503")
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	for _, p := range []string{inDoc, outDoc, sibling} {
		w.handleChange(p) // each fails, and backs off for 30 s
	}
	for _, p := range kept {
		w.requestHydration(p)
	}
	waitUntil(t, "k1 and k2 to start", func() bool { return hydratedHas(f, kept[0]) && hydratedHas(f, kept[1]) })

	res := w.SyncFirst(filepath.Join(root, "Work"))
	if res.Uploads != 1 || res.Downloads != 2 || res.OnlineOnly {
		t.Fatalf("got %+v, want 1 upload and 2 downloads", res)
	}
	wantUpload(t, rec, "Work/report.docx")
	noUpload(t, rec) // the other folders' uploads keep their retry times
	waitUntil(t, "one of the folder's kept files to start", func() bool { return hydratedHas(f, kept[3]) })
	if hydratedHas(f, kept[2]) {
		t.Fatal("another folder's download started ahead of the folder that was asked for")
	}
	if n := f.hydrateCallCount(cloud); n != 0 {
		t.Fatal("an online-only file in the folder was downloaded")
	}

	f.mu.Lock()
	close(f.hydrateGate)
	f.mu.Unlock()
	waitUntil(t, "every kept file to download", func() bool { return f.hydrateCompleted() == len(kept) })
	// (The folder's second file and the other folder's third start together
	// once the two workers are free, so their order says nothing.)
	if a, c := f.eventIndex("hydrate:"+kept[3]), f.eventIndex("hydrate:"+kept[2]); a > c {
		t.Fatalf("download order: Work/k4 at %d, Other/k3 at %d", a, c)
	}
	for _, p := range kept {
		if n := f.hydrateCallCount(p); n != 1 {
			t.Errorf("%s was downloaded %d times, want once", filepath.Base(p), n)
		}
	}
}

func TestSyncFirstRefusesWhatIsNeverSynced(t *testing.T) {
	installFakeCf(t)
	root := t.TempDir()
	lock := filepath.Join(root, "~$report.docx")
	mustWrite(t, lock)
	rec := newRecorder()
	w := bareWatcher(root, rec.ops())
	defer w.cancel()

	for _, p := range []string{lock, filepath.Join(filepath.Dir(root), "elsewhere.txt"), root + "-copy"} {
		if res := w.SyncFirst(p); !res.NotSynced || res.Uploads != 0 {
			t.Errorf("%s: got %+v, want only NotSynced", p, res)
		}
	}
	noUpload(t, rec)
}
