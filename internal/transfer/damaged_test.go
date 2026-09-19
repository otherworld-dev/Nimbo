package transfer

// A server copy whose bytes don't match the checksum stored with it (a torn
// upload from another computer) failed every download, and every pass
// fetched the whole file again: 24 GB, three attempts, every 20 minutes, for
// days (Deck #691). Over HTTPS the bytes cannot change in transit, so a
// mismatch means the server really serves those bytes, and trying again gets
// the same bytes again.

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/otherworld/nimbo/internal/engine"
	"github.com/otherworld/nimbo/internal/state"
	"github.com/otherworld/nimbo/internal/transport"
)

// damagedServer serves "content" for every GET, labelled with the checksum of
// something else, and counts the GETs.
func damagedServer(t *testing.T) (*transport.Client, *atomic.Int32) {
	t.Helper()
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		w.Header().Set("OC-Checksum", "SHA1:4f663abde826ad82d8ff238365e1f9f3e2dd81af")
		w.Header().Set("ETag", `"e1"`)
		_, _ = w.Write([]byte("content"))
	}))
	t.Cleanup(srv.Close)
	return transport.New(srv.URL, "u", "p"), &gets
}

func TestDownloadReportsAChecksumMismatchAsSuch(t *testing.T) {
	c, _ := damagedServer(t)
	_, err := Download(context.Background(), c, "archive.pst", filepath.Join(t.TempDir(), "archive.pst"))
	var cm *ChecksumMismatchError
	if !errors.As(err, &cm) {
		t.Fatalf("err = %v, want a ChecksumMismatchError", err)
	}
	if cm.Want != "4f663abde826ad82d8ff238365e1f9f3e2dd81af" || cm.Got != "040f06fd774092478d450774f5ba30c5da78acc8" {
		t.Fatalf("mismatch detail: got %q want %q", cm.Got, cm.Want)
	}
}

func TestExecutorDoesNotRetryADamagedDownload(t *testing.T) {
	c, gets := damagedServer(t)
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"), "acct", false)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ex := &Executor{Client: c, State: st, PairKey: "P", LocalRoot: t.TempDir()}

	start := time.Now()
	err = ex.applyTransfer(context.Background(), engine.Action{Kind: engine.ActDownload, Path: "archive.pst"})
	var cm *ChecksumMismatchError
	if !errors.As(err, &cm) {
		t.Fatalf("err = %v, want a ChecksumMismatchError", err)
	}
	if n := gets.Load(); n != 1 {
		t.Fatalf("a damaged download was fetched %d times in one pass (took %v)", n, time.Since(start))
	}
}

// A resumed download splices the start of an old version onto the rest of the
// new one when the file changed in between, which for an Outlook .pst on
// another computer is routine. That mismatch is ours, not the server's: start
// again from zero rather than calling a good copy damaged.
func TestAResumedDownloadThatFailsItsChecksumStartsAgain(t *testing.T) {
	content := []byte("NEW-VERSION-0123456789")
	sum := sha1.Sum(content)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("OC-Checksum", "SHA1:"+hex.EncodeToString(sum[:]))
		var from int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &from); err == nil && from > 0 {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, len(content)-1, len(content)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(content[from:])
			return
		}
		_, _ = w.Write(content)
	}))
	t.Cleanup(srv.Close)
	c := transport.New(srv.URL, "u", "p")
	local := filepath.Join(t.TempDir(), "archive.pst")
	if err := os.WriteFile(local+partSuffix, []byte("OLD-"), 0o644); err != nil { // left by an interrupted download
		t.Fatal(err)
	}

	_, err := Download(context.Background(), c, "archive.pst", local)
	var cm *ChecksumMismatchError
	if err == nil || errors.As(err, &cm) {
		t.Fatalf("a spliced resume must fail as retryable, not as a damaged server copy: %v", err)
	}
	if _, serr := os.Stat(local + partSuffix); !os.IsNotExist(serr) {
		t.Fatal("the spliced partial file was kept, so the next try resumes it again")
	}
	if _, err := Download(context.Background(), c, "archive.pst", local); err != nil {
		t.Fatalf("the fresh download: %v", err)
	}
	if b, _ := os.ReadFile(local); string(b) != string(content) {
		t.Fatalf("downloaded %q", b)
	}
}
