package transport

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/otherworld/nimbo/internal/push"
)

// pushMux serves the DAV probe and a notify_push-shaped websocket at /push/ws.
func pushMux(rootID string, rec *recorder, ws *atomic.Int32) http.Handler {
	return pushMuxConn(rootID, rec, ws, nil)
}

// pushMuxConn is pushMux with an optional hook given the accepted websocket
// connection right after the handshake, so a test can force-close that exact
// live session later (simulating the LAN vanishing mid-session). A plain
// server-side Close of the listener does NOT do this on its own: once
// websocket.Accept hijacks the connection, net/http stops tracking it
// entirely (it is a terminal ConnState, removed from the server's tracked
// set), so httptest.Server.Close only stops NEW connections — an
// already-open hijacked one is untouched and keeps blocking on Read.
func pushMuxConn(rootID string, rec *recorder, ws *atomic.Int32, onConn func(*websocket.Conn)) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/push/ws", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws.Add(1)
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		if onConn != nil {
			onConn(c)
		}
		ctx := r.Context()
		_, _, _ = c.Read(ctx) // user
		_, _, _ = c.Read(ctx) // pass
		_ = c.Write(ctx, websocket.MessageText, []byte("authenticated"))
		_ = c.Write(ctx, websocket.MessageText, []byte("notify_file"))
		for ctx.Err() == nil {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	}))
	mux.Handle("/", davHandler(rootID, rec))
	return mux
}

func waitEvent(t *testing.T, ctx context.Context, events <-chan push.Event) {
	t.Helper()
	select {
	case ev := <-events:
		if ev.Type != "notify_file" {
			t.Fatalf("event = %q", ev.Type)
		}
	case <-ctx.Done():
		t.Fatal("no push event")
	}
}

// With the route up, the websocket handshake lands on the LAN server; with an
// impostor certificate it never does, and push connects via public instead.
func TestPushFollowsLocalRoute(t *testing.T) {
	good, impostor := makeCert(t, "cloud.example.com"), makeCert(t, "cloud.example.com")
	trustLocally(t, good)

	t.Run("up", func(t *testing.T) {
		var pubRec, locRec recorder
		var pubWS, locWS atomic.Int32
		pub := newTLSServer(t, certPtr(good), &pubRec, pushMux(testRootID, &pubRec, &pubWS))
		loc := newTLSServer(t, certPtr(good), &locRec, pushMux(testRootID, &locRec, &locWS))
		c := newRoutedTestClient(t, pub, good, loc.Listener.Addr().String(), "", testRootID)
		if err := c.ProbeLocal(ctxT(t)); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		p := push.New("wss://cloud.example.com/push/ws", "alice", "pw")
		p.SetHTTPClient(c.HTTPClient())
		events := make(chan push.Event, 1)
		go p.Run(ctx, func(ev push.Event) { events <- ev })
		waitEvent(t, ctx, events)
		if locWS.Load() != 1 || pubWS.Load() != 0 {
			t.Errorf("websocket hits local=%d public=%d, want 1/0", locWS.Load(), pubWS.Load())
		}
	})

	// The route is forced up (no probe) so the FIRST dial goes local and must
	// be rejected at the TLS handshake itself, by the impostor cert — not by
	// state left over from a prior failed probe. That handshake failure marks
	// the route down; push's own reconnect then lands on public.
	t.Run("impostor", func(t *testing.T) {
		var pubRec, locRec recorder
		var pubWS, locWS atomic.Int32
		pub := newTLSServer(t, certPtr(good), &pubRec, pushMux(testRootID, &pubRec, &pubWS))
		loc := newTLSServer(t, certPtr(impostor), &locRec, pushMux(testRootID, &locRec, &locWS))
		c := newRoutedTestClient(t, pub, good, loc.Listener.Addr().String(), "", testRootID)
		c.router.markUp() // force the first dial LOCAL, straight at the impostor
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		p := push.New("wss://cloud.example.com/push/ws", "alice", "pw")
		p.SetHTTPClient(c.HTTPClient())
		events := make(chan push.Event, 1)
		go p.Run(ctx, func(ev push.Event) { events <- ev })
		waitEvent(t, ctx, events) // arrives via public, after the reconnect
		if route, reason := c.Route(); route != RoutePublic || reason != ReasonCertChanged {
			t.Errorf("Route() = %q, %q", route, reason)
		}
		if locWS.Load() != 0 || pubWS.Load() != 1 {
			t.Errorf("websocket hits local=%d public=%d, want 0/1", locWS.Load(), pubWS.Load())
		}
	})

	// Losing the LAN mid-session: the open local session's connection dies,
	// and the local listener is gone too, so the next redial (still routed
	// local — nothing has marked the route down yet) hits connection-refused,
	// which marks it down; the redial after THAT lands on public.
	t.Run("lan loss mid-session", func(t *testing.T) {
		var pubRec, locRec recorder
		var pubWS, locWS atomic.Int32
		pub := newTLSServer(t, certPtr(good), &pubRec, pushMux(testRootID, &pubRec, &pubWS))
		connCh := make(chan *websocket.Conn, 1)
		loc := newTLSServer(t, certPtr(good), &locRec, pushMuxConn(testRootID, &locRec, &locWS, func(c *websocket.Conn) {
			select {
			case connCh <- c:
			default:
			}
		}))
		c := newRoutedTestClient(t, pub, good, loc.Listener.Addr().String(), "", testRootID)
		if err := c.ProbeLocal(ctxT(t)); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		p := push.New("wss://cloud.example.com/push/ws", "alice", "pw")
		p.SetHTTPClient(c.HTTPClient())
		events := make(chan push.Event, 1)
		go p.Run(ctx, func(ev push.Event) { events <- ev })
		waitEvent(t, ctx, events) // first session, over local
		if locWS.Load() != 1 || pubWS.Load() != 0 {
			t.Fatalf("first session: local=%d public=%d, want 1/0", locWS.Load(), pubWS.Load())
		}

		// The LAN goes away: force-close the live session's own connection
		// (a plain loc.Close() never touches it — see pushMuxConn's doc
		// comment) AND close the listener, so the very next local dial
		// attempt also fails.
		var wsConn *websocket.Conn
		select {
		case wsConn = <-connCh:
		case <-ctx.Done():
			t.Fatal("never captured the local websocket connection")
		}
		wsConn.CloseNow()
		loc.Close()

		waitEvent(t, ctx, events) // second session, after reconnect, over public
		if route, reason := c.Route(); route != RoutePublic || reason != ReasonUnreachable {
			t.Errorf("Route() = %q, %q", route, reason)
		}
		if pubWS.Load() != 1 {
			t.Errorf("public websocket hits = %d, want 1", pubWS.Load())
		}
		select {
		case <-c.RouteDown():
		default:
			t.Error("RouteDown must have a pending signal")
		}
	})
}
