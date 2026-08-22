package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeResp builds an *http.Response with the given status for statusError.
func fakeResp(code int, status string) *http.Response {
	return &http.Response{
		StatusCode: code,
		Status:     status,
		Body:       io.NopCloser(strings.NewReader("<error/>")),
	}
}

// The message format is load-bearing: callers substring-match on
// "server returned <status>" (IsLocked, the GUI's quota hint), so the typed
// error must render exactly what the old fmt.Errorf did.
func TestStatusErrorMessageUnchanged(t *testing.T) {
	err := statusError("PUT", "a/b.txt", fakeResp(507, "507 Insufficient Storage"))
	want := `PUT "a/b.txt": server returned 507 Insufficient Storage: <error/>`
	if err.Error() != want {
		t.Errorf("message = %q, want %q", err.Error(), want)
	}
}

func TestStatusCode(t *testing.T) {
	err := statusError("PUT", "x", fakeResp(502, "502 Bad Gateway"))
	if got := StatusCode(err); got != 502 {
		t.Errorf("StatusCode = %d, want 502", got)
	}
	if got := StatusCode(fmt.Errorf("wrapped: %w", err)); got != 502 {
		t.Errorf("StatusCode(wrapped) = %d, want 502", got)
	}
	if got := StatusCode(errors.New("plain")); got != 0 {
		t.Errorf("StatusCode(plain) = %d, want 0", got)
	}
	if got := StatusCode(nil); got != 0 {
		t.Errorf("StatusCode(nil) = %d, want 0", got)
	}
}

// Retryable steers the chunk-upload retry loop: transient server distress and
// network failures are worth another attempt, deliberate refusals are not.
func TestRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"502", statusError("PUT", "x", fakeResp(502, "502 Bad Gateway")), true},
		{"503", statusError("PUT", "x", fakeResp(503, "503 Service Unavailable")), true},
		{"429 too many requests", statusError("PUT", "x", fakeResp(429, "429 Too Many Requests")), true},
		{"507 quota", statusError("PUT", "x", fakeResp(507, "507 Insufficient Storage")), false},
		{"423 locked", statusError("PUT", "x", fakeResp(423, "423 Locked")), false},
		{"403 forbidden", statusError("PUT", "x", fakeResp(403, "403 Forbidden")), false},
		{"network error", errors.New("read tcp 1.2.3.4: connection reset by peer"), true},
		{"context cancelled", context.Canceled, false},
		{"deadline", context.DeadlineExceeded, false},
		{"wrapped cancel", fmt.Errorf("PUT: %w", context.Canceled), false},
	}
	for _, tc := range cases {
		if got := Retryable(tc.err); got != tc.want {
			t.Errorf("Retryable(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestIsLockedTyped(t *testing.T) {
	if !IsLocked(statusError("PUT", "x", fakeResp(423, "423 Locked"))) {
		t.Error("typed 423 not recognised")
	}
	// The legacy formatted string (errors that crossed a fmt boundary).
	if !IsLocked(errors.New(`PUT "x": server returned 423 Locked: busy`)) {
		t.Error("legacy substring form not recognised")
	}
	if IsLocked(statusError("PUT", "x", fakeResp(500, "500 Internal Server Error"))) {
		t.Error("500 wrongly read as locked")
	}
}

// A chunk PUT must be replayable: the HTTP/2 transport retries a request whose
// connection got a graceful-shutdown GOAWAY only when GetBody can rebuild the
// body (issue #1: "define Request.GetBody to avoid this error" killed a chunked
// upload mid-file on a managed host).
func TestPutChunkRequestIsReplayable(t *testing.T) {
	c := New("https://example.test", "u", "p")
	newBody := func() (io.Reader, error) { return strings.NewReader("chunk-bytes"), nil }
	req, err := c.newChunkRequest(context.Background(), "nimbo-abc", "00001", newBody, 11, "docs/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	if req.GetBody == nil {
		t.Fatal("GetBody not set — an http2 GOAWAY mid-chunk is unretryable without it")
	}
	for i := 0; i < 2; i++ {
		b, err := req.GetBody()
		if err != nil {
			t.Fatalf("GetBody #%d: %v", i+1, err)
		}
		got, _ := io.ReadAll(b)
		if string(got) != "chunk-bytes" {
			t.Fatalf("GetBody #%d read %q, want full chunk from the start", i+1, got)
		}
	}
	if req.ContentLength != 11 {
		t.Errorf("ContentLength = %d, want 11", req.ContentLength)
	}
	if req.Header.Get("Destination") == "" {
		t.Error("v2 Destination header missing")
	}
	// The idempotency marker lets net/http replay the PUT on a stale reused
	// connection too (HTTP/1 path) — Go only retries non-idempotent methods
	// when the header names the request as safe to replay.
	if req.Header.Get("Idempotency-Key") == "" {
		t.Error("Idempotency-Key missing — HTTP/1 dead-connection replays disabled")
	}
}

// PutChunk sends the body produced by the factory and succeeds on 201.
func TestPutChunkSendsBody(t *testing.T) {
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	c := New(srv.URL, "u", "p")
	newBody := func() (io.Reader, error) { return strings.NewReader("payload"), nil }
	if err := c.PutChunk(context.Background(), "id1", "00001", newBody, 7, "d/f.bin"); err != nil {
		t.Fatal(err)
	}
	if string(got) != "payload" {
		t.Errorf("server received %q", got)
	}
}
