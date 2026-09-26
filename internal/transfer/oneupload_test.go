package transfer

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/otherworld/nimbo/internal/engine"
)

// Two uploads of one file at once share its chunk session (the session ID is
// the file's path, size and mtime), so each overwrote the chunks the other was
// sending and whichever assembled first found the server a chunk short: "Chunks
// on server do not sum up to 3048711168 but to 3038225408", exactly one 10 MiB
// chunk (Deck #714). Two sync passes could both be sending a .pst Outlook kept
// touching, and every Keep-mine click started one more. The second must be
// refused at once, not queued behind the first to send the whole file again.
func TestSecondUploadOfTheSameFileIsRefusedWhileTheFirstRuns(t *testing.T) {
	smallChunks(t)
	f, c, local := uploadFixture(t, 3*1024+100)
	inFlight := make(chan struct{})
	release := make(chan struct{})
	var once bool
	f.onChunkPut = func(name string) {
		if !once {
			once = true
			close(inFlight)
			<-release
		}
	}

	first := make(chan error, 1)
	go func() {
		_, err := Upload(context.Background(), c, local, "docs/big.bin")
		first <- err
	}()
	<-inFlight

	done := make(chan error, 1)
	go func() {
		_, err := Upload(context.Background(), c, local, "docs/big.bin")
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrUploadInProgress) {
			t.Fatalf("second upload: err = %v, want ErrUploadInProgress", err)
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("second upload waited for the first instead of being refused")
	}

	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first upload: %v", err)
	}
	f.mu.Lock()
	puts := f.chunkPuts["00001"]
	f.mu.Unlock()
	if puts != 1 {
		t.Errorf("chunk 00001 PUT %d times, want 1", puts)
	}

	// Once the first has finished the file can be uploaded again.
	if _, err := Upload(context.Background(), c, local, "docs/big.bin"); err != nil {
		t.Fatalf("upload after the first finished: %v", err)
	}
}

// A pass that meets a file another pass is uploading leaves it to that one: no
// retries with backoff, no failure counted, and the refusal handed to OnEvent
// so the engine can tell it apart from a real failure.
func TestPassLeavesAFileBeingUploadedToTheUploadThatRuns(t *testing.T) {
	c, _ := conflictServer(t, 0)
	ex := conflictExecutor(t, c)
	local := filepath.Join(ex.LocalRoot, "doc.txt")
	if err := beginUpload(local); err != nil {
		t.Fatal(err)
	}
	defer endUpload(local)
	ex.Workers = 1
	var got error
	ex.OnEvent = func(a engine.Action, err error) { got = err }

	start := time.Now()
	stats, err := ex.Run(context.Background(), []engine.Action{{Kind: engine.ActUpload, Path: "doc.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(got, ErrUploadInProgress) {
		t.Fatalf("OnEvent err = %v, want ErrUploadInProgress", got)
	}
	if stats.Failed != 0 || stats.Uploaded != 0 {
		t.Errorf("stats = %+v, want neither failed nor uploaded", stats)
	}
	if d := time.Since(start); d > 400*time.Millisecond {
		t.Errorf("took %v: the refusal was retried with backoff", d)
	}
}
