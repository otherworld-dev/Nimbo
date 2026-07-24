// Package transport provides the authenticated HTTP layer Nimbo uses to
// talk to a Nextcloud server: a shared client with retry/backoff, plus typed
// helpers for WebDAV (files), OCS (capabilities/notifications) and related
// endpoints.
package transport

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

// Client is an authenticated connection to a single Nextcloud server. It is
// safe for concurrent use.
type Client struct {
	server string // base URL, no trailing slash
	user   string
	pass   string // app password
	hc     *http.Client

	upLimiter   *rate.Limiter // nil = unlimited
	downLimiter *rate.Limiter
}

// userAgent identifies the client in server logs and session lists.
const userAgent = "Nimbo"

// Connection-phase timeouts (vars so tests can shorten them). Per-request
// contexts bound total time where callers set deadlines, but engine
// construction and the mobile facade issue requests on deadline-free
// contexts, so the phases that can silently hang forever on a captive portal
// or half-dead connection need their own bounds. responseHeaderTimeout stays
// generous: after a chunked upload the server may spend minutes assembling
// the file before answering the final MOVE.
var (
	dialTimeout           = 30 * time.Second
	tlsHandshakeTimeout   = 30 * time.Second
	responseHeaderTimeout = 5 * time.Minute
)

// New creates a Client for the given server using basic auth with an app
// password. The underlying http.Client pools connections for efficiency.
func New(server, user, appPassword string) *Client {
	return &Client{
		server: strings.TrimRight(server, "/"),
		user:   user,
		pass:   appPassword,
		hc: &http.Client{
			Timeout: 0, // whole-request deadlines come from context; phases bounded below
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout:   dialTimeout,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				TLSHandshakeTimeout:   tlsHandshakeTimeout,
				ResponseHeaderTimeout: responseHeaderTimeout,
				MaxIdleConns:          100,
				MaxIdleConnsPerHost:   16,
				IdleConnTimeout:       90 * time.Second,
				ForceAttemptHTTP2:     true,
			},
		},
	}
}

// Server returns the base server URL.
func (c *Client) Server() string { return c.server }

// User returns the login name.
func (c *Client) User() string { return c.user }

// NewRequest builds a request to an absolute URL (typically produced by one of
// the endpoint helpers) with auth and the standard user agent applied.
func (c *Client) NewRequest(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.user, c.pass)
	req.Header.Set("User-Agent", userAgent)
	return req, nil
}

// Do executes a request with retry/backoff for transient failures. Retries are
// only applied to idempotent methods (GET, HEAD, PROPFIND) and only when the
// body is nil or rewindable via GetBody (true for the fixed XML bodies built
// from string readers — notably PROPFIND, whose body previously disabled
// retries entirely, so one transient 5xx aborted a whole scan); callers
// performing uploads should use DoOnce.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	if !idempotent(req.Method) || (req.Body != nil && req.GetBody == nil) {
		return c.hc.Do(req)
	}
	const maxAttempts = 4
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			if err := sleep(req.Context(), backoff(attempt)); err != nil {
				return nil, err
			}
		}
		r := req.Clone(req.Context())
		if req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			r.Body = body
		}
		resp, err := c.hc.Do(r)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode >= 500 && resp.StatusCode != http.StatusNotImplemented {
			// A distressed server (500/502/503) gets a breather, honouring
			// Retry-After when it names one (e.g. Nextcloud maintenance mode).
			delay := retryAfter(resp)
			drain(resp)
			lastErr = fmt.Errorf("server returned %s", resp.Status)
			if delay > 0 {
				if err := sleep(req.Context(), delay); err != nil {
					return nil, err
				}
			}
			continue
		}
		return resp, nil
	}
	return nil, fmt.Errorf("request failed after %d attempts: %w", maxAttempts, lastErr)
}

// retryAfter parses a Retry-After response header (delta-seconds form) into a
// wait duration, capped so a hostile value can't stall a sync. 0 = none given.
func retryAfter(resp *http.Response) time.Duration {
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0
	}
	secs, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || secs <= 0 {
		return 0
	}
	if secs > 30 {
		secs = 30
	}
	return time.Duration(secs) * time.Second
}

// DoOnce executes a request exactly once with no retry. Use for non-idempotent
// or streaming requests (PUT uploads, MOVE). Upload bodies are throttled when an
// upload limit is configured.
func (c *Client) DoOnce(req *http.Request) (*http.Response, error) {
	if req.Body != nil && c.upLimiter != nil {
		req.Body = limitReadCloser(req.Context(), req.Body, c.upLimiter)
	}
	return c.hc.Do(req)
}

func idempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, "PROPFIND":
		return true
	default:
		return false
	}
}

// backoff returns an exponential delay for the given (1-based) retry attempt.
func backoff(attempt int) time.Duration {
	d := time.Duration(1<<uint(attempt-1)) * 500 * time.Millisecond
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	return d
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// drain reads and closes a response body so the connection can be reused.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
}
