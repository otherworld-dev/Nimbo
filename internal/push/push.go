// Package push implements a client for the Nextcloud notify_push WebSocket,
// giving real-time events instead of polling: a "notify_file" when something in
// the user's files changed, "notify_notification" for app notifications, and
// "notify_activity" for activity-stream items.
//
// The connection authenticates in-band: after the socket opens, the client
// sends the login name and app password as two text frames and waits for the
// server's "authenticated" acknowledgement. The socket is encrypted (wss), so
// sending the app password over it is safe, and it avoids a separate pre_auth
// round trip.
package push

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// Event is a single message received from the push server.
type Event struct {
	Type string // e.g. "notify_file", "notify_notification", "notify_activity"
	Body string // any text after the type (usually empty)
}

// defaultKeepalivePing is how often we ping the server to keep the WebSocket (and
// its NAT/firewall mapping) alive. It must be shorter than the shortest idle
// timeout in the path — home routers/NATs and reverse proxies commonly drop idle
// connections after ~40-60s — so the socket isn't torn down between events. A
// dropped socket forces a reconnect that re-authenticates against the server,
// which (when it happens on a timer) shows up as periodic Apache/DB load.
const defaultKeepalivePing = 25 * time.Second

// Client maintains a resilient connection to a notify_push WebSocket endpoint.
type Client struct {
	wsURL     string
	user      string
	pass      string
	pingEvery time.Duration        // keepalive ping interval (tests shorten it)
	onStatus  func(connected bool) // optional: connection state for diagnostics
}

// SetStatusFunc registers a callback notified when the connection comes up
// (authenticated) or drops, for surfacing push health in the UI.
func (c *Client) SetStatusFunc(f func(connected bool)) { c.onStatus = f }

func (c *Client) status(up bool) {
	if c.onStatus != nil {
		c.onStatus(up)
	}
}

// New creates a push client for the given wss:// endpoint (from the server
// capabilities) authenticating as user with the app password.
func New(wsURL, user, appPassword string) *Client {
	return &Client{wsURL: wsURL, user: user, pass: appPassword, pingEvery: defaultKeepalivePing}
}

// Run connects and delivers events to onEvent until ctx is cancelled,
// reconnecting with backoff if the connection drops. It returns only when ctx
// ends.
func (c *Client) Run(ctx context.Context, onEvent func(Event)) error {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := c.session(ctx, onEvent)
		c.status(false) // session ended (dropped or failed to connect)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			slog.Warn("push disconnected, reconnecting", "err", err, "in", backoff)
		}
		// A session that stayed up a while was healthy, so a lone drop should
		// recover quickly; reset the backoff. Only genuine flapping (repeated
		// short sessions) keeps backing off, which spares the server.
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
	return nil
}

// session runs a single connection: dial, authenticate, then read until error.
func (c *Client) session(ctx context.Context, onEvent func(Event)) error {
	conn, _, err := websocket.Dial(ctx, c.wsURL, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close(websocket.StatusInternalError, "closing")
	conn.SetReadLimit(1 << 20)

	// In-band auth: username then password, then expect "authenticated".
	if err := conn.Write(ctx, websocket.MessageText, []byte(c.user)); err != nil {
		return err
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(c.pass)); err != nil {
		return err
	}

	// Keep the connection alive so an idle NAT/proxy timeout can't drop it (which
	// would force a re-authenticating reconnect on a timer). Pinging needs the
	// read loop below to process the pong, so it runs concurrently; a failed ping
	// cancels rctx so Read unblocks and Run reconnects.
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		t := time.NewTicker(c.pingEvery)
		defer t.Stop()
		for {
			select {
			case <-rctx.Done():
				return
			case <-t.C:
				pctx, pcancel := context.WithTimeout(rctx, 10*time.Second)
				err := conn.Ping(pctx)
				pcancel()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()

	for {
		_, data, err := conn.Read(rctx)
		if err != nil {
			return err
		}
		msg := strings.TrimSpace(string(data))
		switch {
		case msg == "authenticated":
			slog.Info("push connected")
			c.status(true)
			continue
		case strings.HasPrefix(msg, "err"):
			return fmt.Errorf("push auth failed: %s", msg)
		}
		typ, body, _ := strings.Cut(msg, " ")
		onEvent(Event{Type: typ, Body: body})
	}
}
