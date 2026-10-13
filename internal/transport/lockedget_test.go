package transport

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// shortLockedWaits makes the waits between tries of a 423'd download too short
// to slow a test, and puts the real ones back afterwards.
func shortLockedWaits(t *testing.T, waits ...time.Duration) {
	t.Helper()
	old := LockedGETWaits
	if len(waits) == 0 {
		waits = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	}
	LockedGETWaits = waits
	t.Cleanup(func() { LockedGETWaits = old })
}

// lockedServer answers 423 to the first locked requests of each method and
// then serves body, honouring a "bytes=N-" Range. It records every request.
type lockedServer struct {
	mu     sync.Mutex
	locked int // how many requests still get a 423
	body   string
	seen   []string // "METHOD range" per request, in order
	onReq  func(n int)
}

func (s *lockedServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	s.mu.Lock()
	s.seen = append(s.seen, strings.TrimSpace(r.Method+" "+r.Header.Get("Range")))
	n := len(s.seen)
	refuse := s.locked > 0
	if refuse {
		s.locked--
	}
	hook := s.onReq
	s.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	if refuse {
		w.WriteHeader(http.StatusLocked)
		_, _ = w.Write([]byte(`<d:error><s:message>"f.bin" is locked</s:message></d:error>`))
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusCreated)
		return
	}
	if rng := r.Header.Get("Range"); rng != "" {
		var from int
		for _, ch := range strings.TrimSuffix(strings.TrimPrefix(rng, "bytes="), "-") {
			from = from*10 + int(ch-'0')
		}
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte(s.body[from:]))
		return
	}
	_, _ = w.Write([]byte(s.body))
}

func (s *lockedServer) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

// A 423 to a download is the server's own short lock on the file, not
// somebody's files_lock lock: five of some 1,690 downloads got one during a
// first sync and were left for the next pass, with a reason that blamed a
// colleague (Deck #950). The download waits a moment and asks again.
func TestGetTriesAgainAfterA423(t *testing.T) {
	shortLockedWaits(t)
	s := &lockedServer{locked: 2, body: "the whole file"}
	srv := httptest.NewServer(s)
	defer srv.Close()
	c := New(srv.URL, "adam", "app-password")

	body, _, status, err := c.GetFrom(context.Background(), "f.bin", 0)
	if err != nil {
		t.Fatalf("GetFrom after two 423s: %v", err)
	}
	defer body.Close()
	got, _ := io.ReadAll(body)
	if status != http.StatusOK || string(got) != "the whole file" {
		t.Errorf("status %d, body %q; want 200 and the whole file", status, got)
	}
	if n := len(s.requests()); n != 3 {
		t.Errorf("%d requests, want 3 (two refused, then the file)", n)
	}
}

// A resumed download asks for the same bytes on every try. A 423 carries none
// of the file, so the caller's offset still stands and nothing arrives twice.
func TestGetTriesAgainWithTheSameRange(t *testing.T) {
	shortLockedWaits(t)
	s := &lockedServer{locked: 2, body: "0123456789"}
	srv := httptest.NewServer(s)
	defer srv.Close()
	c := New(srv.URL, "adam", "app-password")

	body, _, status, err := c.GetFrom(context.Background(), "f.bin", 4)
	if err != nil {
		t.Fatalf("ranged GetFrom after two 423s: %v", err)
	}
	defer body.Close()
	got, _ := io.ReadAll(body)
	if status != http.StatusPartialContent || string(got) != "456789" {
		t.Errorf("status %d, body %q; want 206 and the rest of the file", status, got)
	}
	for i, r := range s.requests() {
		if r != "GET bytes=4-" {
			t.Errorf("request %d was %q, want GET bytes=4-", i+1, r)
		}
	}

	// GetRange (what opening an online-only file uses) goes the same way.
	s2 := &lockedServer{locked: 1, body: "0123456789"}
	srv2 := httptest.NewServer(s2)
	defer srv2.Close()
	b2, _, err := New(srv2.URL, "adam", "app-password").GetRange(context.Background(), "f.bin", 0, 0)
	if err != nil {
		t.Fatalf("GetRange after one 423: %v", err)
	}
	defer b2.Close()
	if got, _ := io.ReadAll(b2); string(got) != "0123456789" {
		t.Errorf("GetRange read %q", got)
	}
}

// A server that keeps answering 423 is asked once more per wait and no more,
// and the caller gets the 423 it always got.
func TestGetGivesUpOnA423ThatStays(t *testing.T) {
	shortLockedWaits(t)
	s := &lockedServer{locked: 100}
	srv := httptest.NewServer(s)
	defer srv.Close()
	c := New(srv.URL, "adam", "app-password")

	_, _, _, err := c.GetFrom(context.Background(), "f.bin", 0)
	if !IsLocked(err) || StatusCode(err) != http.StatusLocked {
		t.Fatalf("err = %v, want the 423", err)
	}
	if !LockedDownload(err) {
		t.Error("the 423 to a download is not recognised as one")
	}
	if want := 1 + len(LockedGETWaits); len(s.requests()) != want {
		t.Errorf("%d requests, want %d", len(s.requests()), want)
	}
}

// To a write a 423 is somebody's lock, which is there for as long as they have
// the file open. Asking again only delays saying so.
func TestWritesDoNotTryAgainAfterA423(t *testing.T) {
	shortLockedWaits(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		do   func(c *Client) error
	}{
		{"PUT", func(c *Client) error {
			_, err := c.Put(ctx, "f.bin", strings.NewReader("x"), 1)
			return err
		}},
		{"MOVE", func(c *Client) error { return c.Move(ctx, "f.bin", "g.bin") }},
		{"DELETE", func(c *Client) error { return c.Delete(ctx, "f.bin") }},
		{"LOCK", func(c *Client) error {
			_, err := c.Lock(ctx, "f.bin")
			return err
		}},
	} {
		s := &lockedServer{locked: 100}
		srv := httptest.NewServer(s)
		c := New(srv.URL, "adam", "app-password")
		err := tc.do(c)
		srv.Close()
		if !IsLocked(err) {
			t.Errorf("%s: err = %v, want the 423", tc.name, err)
		}
		if LockedDownload(err) {
			t.Errorf("%s: a refused write reads as a download", tc.name)
		}
		if reqs := s.requests(); len(reqs) != 1 {
			t.Errorf("%s: %d requests %v, want 1", tc.name, len(reqs), reqs)
		}
	}
}

// Stopping the sync ends the wait at once instead of sitting it out.
func TestGetStopsWaitingWhenCancelled(t *testing.T) {
	shortLockedWaits(t, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &lockedServer{locked: 100, onReq: func(int) { cancel() }}
	srv := httptest.NewServer(s)
	defer srv.Close()
	c := New(srv.URL, "adam", "app-password")

	done := make(chan error, 1)
	go func() {
		_, _, _, err := c.GetFrom(ctx, "f.bin", 0)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("still waiting out the 423 after the sync was stopped")
	}
	if n := len(s.requests()); n != 1 {
		t.Errorf("%d requests after cancelling, want 1", n)
	}
}

// The server's own Retry-After is used when it sends one, and the tries run
// out after one per wait.
func TestLockedGETWait(t *testing.T) {
	shortLockedWaits(t, time.Second, 2*time.Second, 4*time.Second)
	plain := &http.Response{Header: http.Header{}}
	for try, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
		if got, ok := lockedGETWait(try, plain); !ok || got != want {
			t.Errorf("wait before try %d = %v, %v; want %v", try+2, got, ok, want)
		}
	}
	if _, ok := lockedGETWait(3, plain); ok {
		t.Error("a fifth try was allowed")
	}
	asked := &http.Response{Header: http.Header{"Retry-After": []string{"3"}}}
	if got, ok := lockedGETWait(0, asked); !ok || got != 3*time.Second {
		t.Errorf("with Retry-After: 3 the wait is %v, %v; want 3s", got, ok)
	}
	if _, ok := lockedGETWait(3, asked); ok {
		t.Error("Retry-After bought a try past the last one")
	}
}
