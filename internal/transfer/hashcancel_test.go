package transfer

// A transfer hashes before it sends: an upload hashes the whole file, a
// resumed upload re-hashes the chunks the server already has, and a resumed
// download re-hashes its .nimbo-part. On a 40-300 GB file that is minutes at
// the start of every attempt, and pausing, setting a large file aside or
// removing its folder waits for the transfer to end (Deck #702). So a hash
// pass stops as soon as its transfer is cancelled.

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/otherworld/nimbo/internal/engine"
	"github.com/otherworld/nimbo/internal/transport"
)

// cancelOnHash returns a context that is cancelled by the first read of a
// hash pass over what, and a count of that pass's reads.
func cancelOnHash(t *testing.T, what string) (context.Context, *atomic.Int32) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var reads atomic.Int32
	testHookHashRead = func(w string) {
		if w == what && reads.Add(1) == 1 {
			cancel()
		}
	}
	t.Cleanup(func() { testHookHashRead = nil; cancel() })
	return ctx, &reads
}

func TestCancellingAnUploadStopsHashingTheFile(t *testing.T) {
	smallChunks(t)
	f, c, local := uploadFixture(t, 1<<20) // 1 MiB: 32 reads' worth
	ctx, reads := cancelOnHash(t, "file")

	_, err := Upload(ctx, c, local, "docs/big.bin")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("the hash read the file %d times after the upload was cancelled, want 1", n)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sessions) != 0 || len(f.chunkPuts) != 0 {
		t.Fatalf("a cancelled upload reached the server: %d sessions, %d chunk PUTs", len(f.sessions), len(f.chunkPuts))
	}
}

func TestCancellingAResumedUploadStopsReHashingItsChunks(t *testing.T) {
	oc, om := chunkThreshold, minChunkSize
	chunkThreshold, minChunkSize = 100, 64<<10 // chunks of two reads each
	t.Cleanup(func() { chunkThreshold, minChunkSize = oc, om })
	f, c, local := uploadFixture(t, 4*(64<<10)+100)
	// Chunks 1 and 2 land; chunk 3 fails past the retry budget.
	f.chunkFail["00003"] = 99
	f.chunkFailCode = http.StatusBadGateway
	if _, err := Upload(context.Background(), c, local, "docs/big.bin"); err == nil {
		t.Fatal("first Upload succeeded, want failure")
	}
	f.mu.Lock()
	f.chunkFail["00003"] = 0
	puts3 := f.chunkPuts["00003"]
	f.mu.Unlock()

	ctx, reads := cancelOnHash(t, "chunk")
	_, err := Upload(ctx, c, local, "docs/big.bin")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("the resume re-hashed %d reads' worth of chunks after it was cancelled, want 1", n)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.chunkPuts["00003"] != puts3 || len(f.files) != 0 {
		t.Fatal("a cancelled resume went on to send or assemble")
	}
}

// rangeServer serves content with Range support and counts the GETs.
func rangeServer(t *testing.T, content []byte) (*transport.Client, *atomic.Int32) {
	t.Helper()
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		w.Header().Set("ETag", `"e1"`)
		http.ServeContent(w, r, "big.bin", time.Unix(1700000000, 0), bytes.NewReader(content))
	}))
	t.Cleanup(srv.Close)
	return transport.New(srv.URL, "u", "p"), &gets
}

func testContent(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

func TestCancellingAResumedDownloadStopsReHashingItsPart(t *testing.T) {
	content := testContent(2 << 20)
	c, gets := rangeServer(t, content)
	local := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(local+partSuffix, content[:1<<20], 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, reads := cancelOnHash(t, "part")

	_, err := Download(ctx, c, "big.bin", local)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("the resume re-hashed %d reads' worth of its part after it was cancelled, want 1", n)
	}
	if n := gets.Load(); n != 0 {
		t.Fatalf("a cancelled download still fetched (%d GETs)", n)
	}
}

func TestCancellingADownloadStopsTheRedundancyHash(t *testing.T) {
	dir := t.TempDir()
	body := testContent(1 << 20)
	if err := os.WriteFile(filepath.Join(dir, "a.bin"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	e := &Executor{LocalRoot: dir, Remote: map[string]engine.RemoteState{
		"a.bin": {Path: "a.bin", Size: int64(len(body)), SHA1: "00000000000000000000000000000000000000aa"},
	}}
	ctx, reads := cancelOnHash(t, "file")
	if _, redundant := e.redundantDownload(ctx, "a.bin"); redundant {
		t.Fatal("redundant = true for a cancelled hash")
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("the hash read the file %d times after the download was cancelled, want 1", n)
	}
}

// A resumed download counts the bytes it already had, as a resumed upload
// counts the chunks the server already has, so a set-aside 40 GB download
// that comes back half done shows half done, not 0 B of 40 GB.
func TestAResumedDownloadReportsTheBytesItAlreadyHad(t *testing.T) {
	content := testContent(256 << 10)
	c, _ := rangeServer(t, content)
	local := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(local+partSuffix, content[:100<<10], 0o644); err != nil {
		t.Fatal(err)
	}
	var total atomic.Int64
	if _, err := DownloadProgress(context.Background(), c, "big.bin", local, func(n int64) { total.Add(n) }); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(local); !bytes.Equal(got, content) {
		t.Fatalf("downloaded file is %d bytes and wrong, want the %d-byte original", len(got), len(content))
	}
	if n := total.Load(); n != int64(len(content)) {
		t.Fatalf("progress reported %d bytes, want %d (the part it resumed from counts)", n, len(content))
	}
}
