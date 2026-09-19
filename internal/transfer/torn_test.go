package transfer

// The checksum an upload declares must be the checksum of the bytes that
// arrive. Nimbo hashed the file, then read it again to send it; a program
// writing in between (Outlook with an attached .pst) made the server keep
// torn bytes under a checksum they did not match, and every other computer
// then failed to download them, forever (Deck #691).

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"os"
	"testing"
)

// scribble writes into local at offset in place, as Outlook does, leaving its
// size alone.
func scribble(t *testing.T, local string, offset int64) {
	t.Helper()
	w, err := os.OpenFile(local, os.O_WRONLY, 0)
	if err != nil {
		t.Errorf("scribble: %v", err)
		return
	}
	defer w.Close()
	if _, err := w.WriteAt([]byte("OUTLOOK"), offset); err != nil {
		t.Errorf("scribble: %v", err)
	}
}

func TestChunkedUploadNeverAssemblesBytesThatChangedWhileSending(t *testing.T) {
	smallChunks(t)
	f, c, local := uploadFixture(t, 3*1024+100)
	t.Cleanup(func() { clearBusyWriter(local) })
	f.onChunkPut = func(name string) {
		if name == "00002" {
			scribble(t, local, 2*1024+10) // into a chunk not sent yet
		}
	}

	_, err := Upload(context.Background(), c, local, "docs/archive.pst")
	var changed *ChangedError
	if !errors.As(err, &changed) {
		t.Fatalf("err = %v, want a ChangedError", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, landed := f.files["docs/archive.pst"]; landed {
		t.Fatal("a torn copy was assembled on the server")
	}
	if len(f.sessions) != 0 {
		t.Fatal("the torn chunks were kept for a later resume to assemble")
	}
}

// A small file goes up in one request. A change after the hash, with its
// modified time put back, gets past the size and mtime check, so the only
// safe answer is to send exactly the bytes that were hashed.
func TestSingleUploadSendsExactlyTheBytesItHashed(t *testing.T) {
	f, c, local := uploadFixture(t, 50)
	fi, err := os.Stat(local)
	if err != nil {
		t.Fatal(err)
	}
	testHookBeforeSend = func() {
		scribble(t, local, 10)
		if err := os.Chtimes(local, fi.ModTime(), fi.ModTime()); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { testHookBeforeSend = nil })

	if _, err := Upload(context.Background(), c, local, "docs/small.bin"); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	sum := sha1.Sum(f.files["docs/small.bin"])
	if want := ocChecksum(hex.EncodeToString(sum[:])); f.declared["docs/small.bin"] != want {
		t.Fatalf("declared %q for bytes whose checksum is %q", f.declared["docs/small.bin"], want)
	}
}
