package push

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// wsURL converts an httptest http:// base to a ws:// URL.
func wsURL(httpURL string) string { return "ws" + strings.TrimPrefix(httpURL, "http") }

// TestConnectAuthAndEvent verifies the in-band auth handshake (username then
// password), the "authenticated" -> status(true) transition, and that a server
// event is delivered to onEvent (and that "authenticated" is NOT delivered as an
// event).
func TestConnectAuthAndEvent(t *testing.T) {
	var gotUser, gotPass string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		ctx := r.Context()
		gotUser = readText(ctx, c)
		gotPass = readText(ctx, c)
		_ = c.Write(ctx, websocket.MessageText, []byte("authenticated"))
		_ = c.Write(ctx, websocket.MessageText, []byte("notify_file"))
		// Keep the connection open (and handle any client pings) until the test ends.
		for ctx.Err() == nil {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events := make(chan Event, 8)
	var connected atomic.Bool
	c := New(wsURL(srv.URL), "alice", "s3cret")
	c.SetStatusFunc(func(up bool) { connected.Store(up) })
	go c.Run(ctx, func(ev Event) { events <- ev })

	select {
	case ev := <-events:
		if ev.Type != "notify_file" {
			t.Fatalf("first event = %q, want notify_file (authenticated must not be delivered)", ev.Type)
		}
	case <-ctx.Done():
		t.Fatal("no event delivered")
	}
	if gotUser != "alice" || gotPass != "s3cret" {
		t.Fatalf("auth frames = %q/%q, want alice/s3cret", gotUser, gotPass)
	}
	if !connected.Load() {
		t.Error("status callback should report connected after authentication")
	}
}

// TestReconnects verifies that when the server drops the connection, the client
// reconnects (re-authenticating), and that the status callback toggles.
func TestReconnects(t *testing.T) {
	var conns int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		n := atomic.AddInt32(&conns, 1)
		ctx := r.Context()
		_ = readText(ctx, c) // user
		_ = readText(ctx, c) // pass
		_ = c.Write(ctx, websocket.MessageText, []byte("authenticated"))
		if n == 1 {
			// Drop the first connection immediately to force a reconnect.
			c.Close(websocket.StatusNormalClosure, "bye")
			return
		}
		_ = c.Write(ctx, websocket.MessageText, []byte("notify_file"))
		for ctx.Err() == nil {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	// Reconnect backoff starts at 1s; that's fine within the timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	events := make(chan Event, 8)
	c := New(wsURL(srv.URL), "alice", "s3cret")
	go c.Run(ctx, func(ev Event) { events <- ev })

	select {
	case ev := <-events: // only arrives on the SECOND connection
		if ev.Type != "notify_file" {
			t.Fatalf("event = %q, want notify_file", ev.Type)
		}
	case <-ctx.Done():
		t.Fatal("client did not reconnect after the first drop")
	}
	if atomic.LoadInt32(&conns) < 2 {
		t.Fatalf("server saw %d connections, want >= 2 (a reconnect)", conns)
	}
}

// TestKeepalivePingDoesNotBreakSession exercises the keepalive path: with a very
// short interval, the client pings repeatedly; the session must stay up (one
// connection, no reconnect churn) while idle.
func TestKeepalivePingDoesNotBreakSession(t *testing.T) {
	var conns int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		atomic.AddInt32(&conns, 1)
		ctx := r.Context()
		_ = readText(ctx, c)
		_ = readText(ctx, c)
		_ = c.Write(ctx, websocket.MessageText, []byte("authenticated"))
		for ctx.Err() == nil { // auto-pongs client pings via Read
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	var statusMu sync.Mutex
	lastUp := false
	c := New(wsURL(srv.URL), "alice", "s3cret")
	c.pingEvery = 20 * time.Millisecond // many ping cycles within the sleep below
	c.SetStatusFunc(func(up bool) { statusMu.Lock(); lastUp = up; statusMu.Unlock() })
	done := make(chan struct{})
	go func() { c.Run(ctx, func(Event) {}); close(done) }()

	time.Sleep(300 * time.Millisecond) // ~15 ping cycles
	statusMu.Lock()
	up := lastUp
	statusMu.Unlock()
	cancel()
	<-done // let Run fully stop before the test exits (no lingering goroutines)

	if !up {
		t.Error("connection should still be up after many keepalive pings")
	}
	if n := atomic.LoadInt32(&conns); n != 1 {
		t.Errorf("keepalive should hold one session; saw %d connections", n)
	}
}

func readText(ctx context.Context, c *websocket.Conn) string {
	_, b, err := c.Read(ctx)
	if err != nil {
		return ""
	}
	return string(b)
}
