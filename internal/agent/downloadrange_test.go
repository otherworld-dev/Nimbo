package agent

// DownloadRange hydrates on-demand placeholders (Windows cfapi fetchData
// callback). It used to ride GetFrom's open-ended "Range: bytes=off-" and get
// its stream abandoned after each caller-sized chunk — thousands of requests
// for a large file. It is now built on the engine's OpenRange, which sends a
// single bounded Range covering exactly the requested span.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDownloadRangeSendsABoundedRange(t *testing.T) {
	var gotRange string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.Header.Get("Range")
		w.Header().Set("Content-Range", "bytes 10-19/100")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("0123456789"))
	}))
	defer srv.Close()
	e, _ := newHookEngine(t, srv.URL)

	got, err := e.DownloadRange(context.Background(), "doc.bin", 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	if gotRange != "bytes=10-19" {
		t.Errorf("Range = %q, want bytes=10-19 (bounded, not the old open-ended bytes=10-)", gotRange)
	}
	if string(got) != "0123456789" {
		t.Errorf("got %q, want %q", got, "0123456789")
	}
}

// The server may send more than asked (some don't bother trimming to the
// range); DownloadRange must still hand back exactly length bytes.
func TestDownloadRangeTrimsAnOverlongServerResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-9/100")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("0123456789EXTRA"))
	}))
	defer srv.Close()
	e, _ := newHookEngine(t, srv.URL)

	got, err := e.DownloadRange(context.Background(), "doc.bin", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "0123456789" {
		t.Errorf("got %q, want %q (trimmed to length)", got, "0123456789")
	}
}

// TestOpenRangeTrimsAnOverlongServerResponse covers OpenRange's own
// io.LimitReader directly (DownloadRange's io.ReadFull already bounds the
// read regardless, so it passes even without the limiter — this test reads
// with io.ReadAll instead, which would happily read past length if the
// limiter were missing or miswired).
func TestOpenRangeTrimsAnOverlongServerResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-9/100")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("0123456789EXTRA"))
	}))
	defer srv.Close()
	e, _ := newHookEngine(t, srv.URL)

	rc, err := e.OpenRange(context.Background(), "doc.bin", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "0123456789" {
		t.Errorf("got %q, want %q (limited to length, not the overlong server body)", got, "0123456789")
	}
}
