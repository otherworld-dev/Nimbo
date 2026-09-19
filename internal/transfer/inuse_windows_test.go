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
