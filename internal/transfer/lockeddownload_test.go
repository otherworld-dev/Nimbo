package transfer

// A 423 to a download is the server's own short lock on the file while another
// request touches it, not a colleague's lock: five of some 1,690 downloads got
// one during a first sync running 32 at a time, and were left for a later pass
// (Deck #950). The download is asked for again after a moment.

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/otherworld/nimbo/internal/engine"
	"github.com/otherworld/nimbo/internal/state"
	"github.com/otherworld/nimbo/internal/transport"
)

// brieflyLocked serves content, with its checksum, after answering 423 to the
// first locked GETs. It honours "Range: bytes=N-" and records every Range it
// was sent ("" for none).
type brieflyLocked struct {
	mu      sync.Mutex
	locked  int
	content []byte
	ranges  []string
}

func (s *brieflyLocked) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.ranges = append(s.ranges, r.Header.Get("Range"))
	refuse := s.locked > 0
	if refuse {
		s.locked--
	}
	s.mu.Unlock()
	if refuse {
		w.WriteHeader(http.StatusLocked)
		_, _ = w.Write([]byte(`<d:error><s:message>"report.pdf" is locked</s:message></d:error>`))
		return
	}
	sum := sha1.Sum(s.content)
	w.Header().Set("OC-Checksum", "SHA1:"+hex.EncodeToString(sum[:]))
	w.Header().Set("ETag", `"e1"`)
	var from int
	if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &from); err == nil && from > 0 {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, len(s.content)-1, len(s.content)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(s.content[from:])
		return
	}
	_, _ = w.Write(s.content)
}

func (s *brieflyLocked) asked() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ranges...)
}

func brieflyLockedServer(t *testing.T, locked int, content string) (*brieflyLocked, *transport.Client) {
	t.Helper()
	old := transport.LockedGETWaits
	transport.LockedGETWaits = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { transport.LockedGETWaits = old })
	s := &brieflyLocked{locked: locked, content: []byte(content)}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return s, transport.New(srv.URL, "u", "p")
}

func TestDownloadTriesAgainAfterA423(t *testing.T) {
	s, c := brieflyLockedServer(t, 2, "the quarterly report")
	local := filepath.Join(t.TempDir(), "report.pdf")

	res, err := Download(context.Background(), c, "report.pdf", local)
	if err != nil {
		t.Fatalf("download after two 423s: %v", err)
	}
	if b, _ := os.ReadFile(local); string(b) != "the quarterly report" {
		t.Fatalf("downloaded %q", b)
	}
	if res.Size != int64(len("the quarterly report")) || res.ETag != "e1" {
		t.Errorf("result = %+v", res)
	}
	if n := len(s.asked()); n != 3 {
		t.Errorf("%d GETs, want 3 (two refused, then the file)", n)
	}
	if _, err := os.Stat(local + partSuffix); !os.IsNotExist(err) {
		t.Error("a partial file was left behind")
	}
}

// A download picking up a partial file asks for the same bytes on every try,
// and the part on disk is not touched in between: the finished file is the
// server's, byte for byte, which the checksum the server sends confirms.
func TestAResumedDownloadTriesAgainAfterA423WithoutRepeatingBytes(t *testing.T) {
	const content = "0123456789-the-rest-of-the-file"
	s, c := brieflyLockedServer(t, 2, content)
	local := filepath.Join(t.TempDir(), "report.pdf")
	if err := os.WriteFile(local+partSuffix, []byte(content[:10]), 0o644); err != nil { // left by an interrupted download
		t.Fatal(err)
	}

	if _, err := Download(context.Background(), c, "report.pdf", local); err != nil {
		t.Fatalf("resumed download after two 423s: %v", err)
	}
	if b, _ := os.ReadFile(local); string(b) != content {
		t.Fatalf("downloaded %q, want %q", b, content)
	}
	asked := s.asked()
	if len(asked) != 3 {
		t.Fatalf("%d GETs %v, want 3", len(asked), asked)
	}
	for i, r := range asked {
		if r != "bytes=10-" {
			t.Errorf("GET %d asked for %q, want bytes=10-", i+1, r)
		}
	}
}

// When the server keeps the file locked past the last try, the download fails
// with the 423 and the pass moves on: the executor does not start the tries
// over again on top of the transport's.
func TestExecutorDoesNotStartOverOnA423ThatStays(t *testing.T) {
	s, c := brieflyLockedServer(t, 1000, "never served")
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"), "acct", false)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	root := t.TempDir()
	ex := &Executor{Client: c, State: st, PairKey: "P", LocalRoot: root}

	err = ex.applyTransfer(context.Background(), engine.Action{Kind: engine.ActDownload, Path: "report.pdf"})
	if !transport.LockedDownload(err) {
		t.Fatalf("err = %v, want the download's 423", err)
	}
	if want := 1 + len(transport.LockedGETWaits); len(s.asked()) != want {
		t.Errorf("%d GETs, want %d", len(s.asked()), want)
	}
	if _, err := os.Stat(filepath.Join(root, "report.pdf")); !os.IsNotExist(err) {
		t.Error("a file was written for a download that never arrived")
	}
}
