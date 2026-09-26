package transfer

// Outlook writes to an attached .pst whenever it is open, so an upload read
// bytes that moved under it and the server kept a torn copy (Deck #691). A
// program merely HOLDING a file open to write is not that: Word and Excel keep
// every open document so and write only on save, and those saves must go on
// uploading while the document stays open.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/otherworld/nimbo/internal/engine"
	"github.com/otherworld/nimbo/internal/state"
	"github.com/otherworld/nimbo/internal/transport"
)

func TestUploadSendsADocumentAnotherAppHoldsOpen(t *testing.T) {
	f, c, local := uploadFixture(t, 50)
	word, err := os.OpenFile(local, os.O_RDWR, 0) // open, but not writing
	if err != nil {
		t.Fatal(err)
	}
	defer word.Close()

	if _, err := Upload(context.Background(), c, local, "docs/report.docx"); err != nil {
		t.Fatalf("a document held open (not being written) was refused: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.files["docs/report.docx"]) != 50 {
		t.Fatal("the document was not uploaded")
	}
}

// Once a file has been caught changing mid-upload, it is not read again while
// something still holds it open to write: every such attempt would burn the
// whole file's worth of bandwidth for nothing. It uploads once that is over.
func TestUploadWaitsForAWriterItCaughtChangingTheFile(t *testing.T) {
	smallChunks(t)
	f, c, local := uploadFixture(t, 3*1024+100)
	f.onChunkPut = func(name string) {
		if name == "00002" {
			scribble(t, local, 2*1024+10) // into a chunk not sent yet
		}
	}
	var changed *ChangedError
	if _, err := Upload(context.Background(), c, local, "docs/archive.pst"); !errors.As(err, &changed) {
		t.Fatalf("first attempt: err = %v, want a ChangedError", err)
	}
	f.mu.Lock()
	f.onChunkPut = nil
	putsBefore := len(f.chunkPuts)
	f.mu.Unlock()

	outlook, err := os.OpenFile(local, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Upload(context.Background(), c, local, "docs/archive.pst")
	var inUse *InUseError
	if !errors.As(err, &inUse) {
		outlook.Close()
		t.Fatalf("with the writer still there: err = %v, want an InUseError", err)
	}
	if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Errorf("InUseError must still read as a sharing violation (on-demand mode's busy check): %v", err)
	}
	f.mu.Lock()
	sent := len(f.chunkPuts) != putsBefore
	f.mu.Unlock()
	if sent {
		t.Error("a file still being written was read and sent again")
	}

	outlook.Close()
	if _, err := Upload(context.Background(), c, local, "docs/archive.pst"); err != nil {
		t.Fatalf("after the writer closed: %v", err)
	}
}

// On-demand mode moves a changed server copy aside BEFORE uploading, so it
// must be able to ask first whether the upload would be put off, or the
// server path would stay empty for as long as Outlook stays open.
func TestUploadDeferredAnswersBeforeAnythingIsTouched(t *testing.T) {
	_, _, local := uploadFixture(t, 50)
	t.Cleanup(func() { clearBusyWriter(local) })
	writer, err := os.OpenFile(local, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()

	if err := UploadDeferred(local); err != nil {
		t.Fatalf("a file never caught changing is not deferred, however it is held: %v", err)
	}
	markBusyWriter(local)
	var inUse *InUseError
	if err := UploadDeferred(local); !errors.As(err, &inUse) {
		t.Fatalf("caught changing and still held open to write: err = %v, want an InUseError", err)
	}
	writer.Close()
	if err := UploadDeferred(local); err != nil {
		t.Fatalf("once the writer has gone: %v", err)
	}
}

// "In use" is not a transient failure: Outlook keeps a .pst open for hours, so
// retrying inside the pass only delays the message. Two retries' backoff is at
// least 1.5s; one try is instant.
func TestExecutorDoesNotRetryAFileInUse(t *testing.T) {
	_, c, local := uploadFixture(t, 50)
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"), "acct", false)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	markBusyWriter(local)
	t.Cleanup(func() { clearBusyWriter(local) })
	writer, err := os.OpenFile(local, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	ex := &Executor{Client: c, State: st, PairKey: "P", LocalRoot: filepath.Dir(local)}

	start := time.Now()
	err = ex.applyTransfer(context.Background(), engine.Action{Kind: engine.ActUpload, Path: filepath.Base(local)})
	var inUse *InUseError
	if !errors.As(err, &inUse) {
		t.Fatalf("err = %v, want an InUseError", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("a file in use was retried within the pass (took %v)", d)
	}
}

// Nimbo's reads must not block a safe save the way Word does one: move the
// original aside, move the new file into its name, delete the original. Go's
// os.Open doesn't share delete, so while an upload held the file open (minutes,
// for a large one) the first step failed and the editor couldn't save.
func TestASafeSaveDuringAnUploadIsNotBlocked(t *testing.T) {
	smallChunks(t)
	f, c, local := uploadFixture(t, 3*1024+100)
	var saveErr error
	saved := false
	f.onChunkPut = func(name string) {
		if name != "00002" || saved {
			return
		}
		saved = true
		tmp, old := local+".saving", local+".old"
		if saveErr = os.WriteFile(tmp, make([]byte, 3*1024+100), 0o644); saveErr != nil {
			return
		}
		if saveErr = os.Rename(local, old); saveErr != nil {
			return
		}
		if saveErr = os.Rename(tmp, local); saveErr != nil {
			return
		}
		saveErr = os.Remove(old)
	}
	if _, err := Upload(context.Background(), c, local, "docs/report.docx"); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if saveErr != nil {
		t.Fatalf("saving the file during the upload failed: %v", saveErr)
	}
}

// A save that REPLACES the file while it is being hashed is an editor's atomic
// save, not a program writing into the file, and must not mark the file as
// written in place: that would hold every later upload of a Word document
// back until Word closed it.
func TestAReplacedFileIsNotTakenForAnInPlaceWriter(t *testing.T) {
	smallChunks(t)
	_, c, local := uploadFixture(t, 3*1024+100)
	t.Cleanup(func() { clearBusyWriter(local) })
	testHookBeforeSend = func() { // hashed; not yet opened to send
		tmp := local + ".saving"
		if err := os.WriteFile(tmp, make([]byte, 3*1024+100), 0o644); err != nil {
			t.Error(err)
			return
		}
		if err := os.Rename(tmp, local); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { testHookBeforeSend = nil })
	_, _ = Upload(context.Background(), c, local, "docs/report.docx")
	if isBusyWriter(local) {
		t.Fatal("an atomic save marked the file as written in place")
	}
}

// Uploads and hashing open files through openShared, which must cope with a
// path of 248 characters or more (Explorer makes those happily): a malformed
// \?\ prefix made every such file fail to upload, and a rename into a deep
// folder then went to the server as a delete with no upload.
func TestALongPathHashesAndUploads(t *testing.T) {
	f, c, _ := uploadFixture(t, 50)
	dir := t.TempDir()
	for len(dir) < 270 {
		dir = filepath.Join(dir, "a-fairly-long-folder-name")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(dir, "report.docx")
	if err := os.WriteFile(local, []byte("long path content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := SHA1File(local); err != nil {
		t.Fatalf("hashing a %d-character path: %v", len(local), err)
	}
	if _, err := Upload(context.Background(), c, local, "docs/report.docx"); err != nil {
		t.Fatalf("uploading a %d-character path: %v", len(local), err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if string(f.files["docs/report.docx"]) != "long path content" {
		t.Fatal("the long-path file was not uploaded")
	}
}

// pstFixture is uploadFixture's file renamed to an Outlook data file.
func pstFixture(t *testing.T, name string) (*fakeNC, *transport.Client, string) {
	t.Helper()
	f, c, local := uploadFixture(t, 50)
	renamed := filepath.Join(filepath.Dir(local), name)
	if err := os.Rename(local, renamed); err != nil {
		t.Fatal(err)
	}
	return f, c, renamed
}

// Deck #634. Outlook writes to an attached .pst in bursts, not constantly, so
// a burst that lands BETWEEN two uploads is never caught mid-read. The file
// was then never marked as having a busy writer, and every burst sent the
// whole file again: 3 GB and 24 GB archives, re-uploaded all day while
// Outlook stayed open. An Outlook data file is held from the very first
// attempt while any program has it open to write, and uploads once when
// that program lets go.
func TestUploadDeferredHoldsAnOutlookDataFileFromTheFirstAttempt(t *testing.T) {
	for _, name := range []string{"OnlineArchive.pst", "MAILBOX.PST", "cache.ost"} {
		t.Run(name, func(t *testing.T) {
			_, _, local := pstFixture(t, name)
			outlook, err := os.OpenFile(local, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer outlook.Close()

			var inUse *InUseError
			held := UploadDeferred(local)
			if !errors.As(held, &inUse) {
				t.Fatalf("held open by Outlook, never caught changing: err = %v, want an InUseError", held)
			}
			if !errors.Is(held, windows.ERROR_SHARING_VIOLATION) {
				t.Errorf("InUseError must still read as a sharing violation (on-demand mode's busy check): %v", held)
			}
			outlook.Close()
			if err := UploadDeferred(local); err != nil {
				t.Fatalf("once Outlook has closed it: %v", err)
			}
		})
	}
}

// And the upload itself is refused before a byte is read or sent.
func TestUploadDoesNotSendAnOutlookDataFileHeldOpen(t *testing.T) {
	f, c, local := pstFixture(t, "OnlineArchive.pst")
	outlook, err := os.OpenFile(local, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer outlook.Close()

	var inUse *InUseError
	if _, err := Upload(context.Background(), c, local, "To Sort/OnlineArchive.pst"); !errors.As(err, &inUse) {
		t.Fatalf("err = %v, want an InUseError", err)
	}
	f.mu.Lock()
	sent := len(f.files["To Sort/OnlineArchive.pst"]) != 0
	f.mu.Unlock()
	if sent {
		t.Fatal("the archive was uploaded while Outlook had it open")
	}
}

// Every other file keeps the old rule: held open is fine, only a file caught
// changing mid-upload waits (Word and Excel hold documents open to write).
func TestUploadDeferredStillLetsAnOpenDocumentGo(t *testing.T) {
	_, _, local := pstFixture(t, "report.docx")
	word, err := os.OpenFile(local, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer word.Close()
	if err := UploadDeferred(local); err != nil {
		t.Fatalf("an open document was held: %v", err)
	}
}
