package transport

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Local network route — docs/specs/2026-09-13-local-network-route-design.md.
//
// The account keeps one URL. While the local route is up, a connection to the
// account host is dialled to the LAN address instead; the Host header, SNI,
// certificate name check, cookies and every URL stay the public ones. It is
// split-horizon DNS scoped to one account, done inside this client.

// Route names (Client.Route, ForceRoute).
const (
	RoutePublic = "public"
	RouteLocal  = "local"
)

// Why the local route is not in use (Client.Route's second value).
const (
	ReasonUnreachable = "unreachable"
	ReasonCertChanged = "certificate changed"
	ReasonDifferent   = "different server"
)

var (
	localDialTimeout      = 3 * time.Second  // a LAN host answers fast or isn't there
	localHandshakeTimeout = 10 * time.Second // generous for a small NAS doing RSA
	probeTimeout          = 5 * time.Second  // whole ProbeLocal, dial included
	// localRootCAs overrides the verified-mode trust store. Tests only; nil =
	// the OS store (on Windows, the Windows certificate store).
	localRootCAs *x509.CertPool
)

type routeKey struct{}

// ForceRoute returns a context that makes the router send the request on the
// named route regardless of the local route's state (host rule still applies).
// The prober needs RouteLocal while the route is down; setup's same-server
// comparison needs one request on each.
func ForceRoute(ctx context.Context, route string) context.Context {
	return context.WithValue(ctx, routeKey{}, route)
}

// PinMismatchError is a pinned local endpoint presenting a different
// certificate. It fails the TLS handshake, so no request bytes (and no
// credentials) were sent.
type PinMismatchError struct{ Want, Got string }

func (e *PinMismatchError) Error() string {
	return fmt.Sprintf("local endpoint certificate changed: fingerprint %s, pinned %s", e.Got, e.Want)
}

// Fingerprint is the SHA-256 of a certificate's DER bytes as lowercase hex —
// what browsers and `openssl x509 -fingerprint -sha256` show, minus colons.
func Fingerprint(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	return hex.EncodeToString(sum[:])
}

// router is the Client's RoundTripper: the public transport plus an optional
// local one. It returns responses untouched — never wrap resp.Body here; the
// websocket handshake needs the transport's own writable 101 body.
type router struct {
	public *http.Transport
	host   string // account host:port (hostPort form) — the only host ever redirected

	mu     sync.Mutex
	local  *http.Transport // nil = no local route configured
	addr   string          // dial target, host:port
	rootID string          // oc:id the local endpoint must report
	up     bool
	reason string
	downCh chan struct{} // one buffered signal per up→down flip (Client.RouteDown)
}

func newRouter(public *http.Transport, serverURL string) *router {
	return &router{public: public, host: hostPort(serverURL), downCh: make(chan struct{}, 1)}
}

// hostPortOf normalises a URL's authority to host:port with the scheme's
// default port filled in, so "https://x" and "https://x:443" compare equal.
func hostPortOf(u *url.URL) string {
	h := strings.ToLower(u.Hostname())
	if strings.Contains(h, ":") {
		h = "[" + h + "]"
	}
	p := u.Port()
	if p == "" {
		if u.Scheme == "http" {
			p = "80"
		} else {
			p = "443"
		}
	}
	return h + ":" + p
}

func hostPort(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return hostPortOf(u)
}

// withDefaultPort appends port when addr has none. Bracketed IPv6 without a
// port ("[fd00::1]") fails SplitHostPort just like a bare name, so both get it.
func withDefaultPort(addr, port string) string {
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	return addr + ":" + port
}

// pick chooses the transport for req and reports whether it is the local one.
func (r *router) pick(req *http.Request) (*http.Transport, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.local == nil {
		return r.public, false
	}
	forced, _ := req.Context().Value(routeKey{}).(string)
	if forced == RoutePublic || hostPortOf(req.URL) != r.host {
		return r.public, false
	}
	if forced == RouteLocal || r.up {
		return r.local, true
	}
	return r.public, false
}

// RoundTrip forwards to the picked transport. A connection-level failure on
// the local transport marks the route down; the request itself is NOT
// replayed here — the callers' existing retries resend it and it goes public.
func (r *router) RoundTrip(req *http.Request) (*http.Response, error) {
	tr, isLocal := r.pick(req)
	resp, err := tr.RoundTrip(req)
	if err != nil && isLocal && req.Context().Err() == nil && connectionError(err) {
		r.markDown(reasonFor(err))
	}
	return resp, err
}

// CloseIdleConnections forwards to both transports (http.Client calls it only
// if the RoundTripper has it; otherwise the call silently does nothing).
func (r *router) CloseIdleConnections() {
	r.public.CloseIdleConnections()
	r.mu.Lock()
	l := r.local
	r.mu.Unlock()
	if l != nil {
		l.CloseIdleConnections()
	}
}

// markDown flips to public, records why, drops pooled LAN connections and
// wakes the prober — once per flip; repeated failures while down only refresh
// the reason.
func (r *router) markDown(reason string) {
	r.mu.Lock()
	wasUp := r.up
	r.up, r.reason = false, reason
	l := r.local
	r.mu.Unlock()
	if l != nil {
		l.CloseIdleConnections()
	}
	if wasUp {
		select {
		case r.downCh <- struct{}{}:
		default:
		}
	}
}

func (r *router) markUp() {
	r.mu.Lock()
	r.up, r.reason = true, ""
	r.mu.Unlock()
	r.public.CloseIdleConnections() // don't linger on the internet path
}

func (r *router) state() (route, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.local != nil && r.up {
		return RouteLocal, ""
	}
	return RoutePublic, r.reason
}

// setLocal installs (addr == "" removes) the local transport. The route starts
// DOWN — only a successful probe brings it up. Safe while requests are in
// flight: the old transport just loses its idle pool.
func (r *router) setLocal(addr, pin, rootID string) {
	var t *http.Transport
	if addr != "" {
		t = newLocalTransport(addr, pin)
	}
	r.mu.Lock()
	old := r.local
	r.local, r.addr, r.rootID, r.up, r.reason = t, addr, rootID, false, ""
	r.mu.Unlock()
	if old != nil {
		old.CloseIdleConnections()
	}
}

// newLocalTransport mirrors the public transport's settings but dials addr
// whatever host the request names, on a short timeout, and — with a pin —
// swaps chain verification for an exact leaf-fingerprint match.
func newLocalTransport(addr, pin string) *http.Transport {
	d := &net.Dialer{Timeout: localDialTimeout, KeepAlive: 30 * time.Second}
	// ServerName is left empty on purpose: Go fills it from the request host,
	// and this transport only ever carries the account host, so SNI and the
	// name check are the public hostname automatically.
	tlsConf := &tls.Config{RootCAs: localRootCAs}
	if pin != "" {
		// Pinned mode. InsecureSkipVerify only switches off the chain+hostname
		// check; VerifyConnection replaces it with an exact match on the leaf's
		// SHA-256, run inside the handshake before any request bytes. The two
		// MUST stay together — TestPinnedModeRejectsOtherCert fails if the
		// VerifyConnection is ever dropped. This is Go's standard pinning shape.
		want := strings.ToLower(pin)
		tlsConf.InsecureSkipVerify = true
		tlsConf.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return &PinMismatchError{Want: want, Got: "none"}
			}
			if got := Fingerprint(cs.PeerCertificates[0]); got != want {
				return &PinMismatchError{Want: want, Got: got}
			}
			return nil
		}
	}
	return &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return d.DialContext(ctx, network, addr)
		},
		TLSClientConfig:       tlsConf,
		TLSHandshakeTimeout:   localHandshakeTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true, // a custom TLSClientConfig otherwise turns HTTP/2 off
	}
}

// connectionError reports whether err means the connection itself failed —
// dial, TLS, pin, reset — as opposed to the caller giving up (context) or a
// request body that couldn't be read. Only the former marks the local route
// down. HTTP statuses never reach here: RoundTrip returns them as responses.
//
// err is unwrapped through a *url.Error first (http.Client.Do wraps every
// RoundTripper error that way) — *url.Error itself satisfies net.Error by
// forwarding Timeout()/Temporary() to whatever it wraps, so checking the
// outer value directly would match on ANY cause, including a wrapped
// StatusError or context error.
func connectionError(err error) bool {
	if err == nil {
		return false
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		err = ue.Err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var (
		pm  *PinMismatchError
		cve *tls.CertificateVerificationError
		rhe tls.RecordHeaderError
		ne  net.Error
	)
	switch {
	case errors.As(err, &pm), errors.As(err, &cve), errors.As(err, &rhe), errors.As(err, &ne):
		return true
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return true
	}
	return false
}

// reasonFor maps a connection error to the reason the UI shows.
func reasonFor(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		err = ue.Err
	}
	var pm *PinMismatchError
	var cve *tls.CertificateVerificationError
	if errors.As(err, &pm) || errors.As(err, &cve) {
		return ReasonCertChanged
	}
	return ReasonUnreachable
}

// ---- Client API ----

// SetLocalRoute installs a local network address for this account's host
// (addr == "" removes it). addr may omit the port; the account URL's is used.
// pin ("" = OS trust store) and rootID come from setup. The route starts DOWN:
// only ProbeLocal brings it up. Refused on http:// accounts — over plain HTTP
// an impostor at the same LAN IP on another network would receive the app
// password before the same-server check could run.
func (c *Client) SetLocalRoute(addr, pin, rootID string) error {
	if addr != "" {
		if !strings.HasPrefix(strings.ToLower(c.server), "https://") {
			return errors.New("a local network address needs an https:// account")
		}
		_, port, _ := net.SplitHostPort(c.router.host)
		addr = withDefaultPort(addr, port)
	}
	c.router.setLocal(addr, pin, rootID)
	return nil
}

// HasLocalRoute reports whether SetLocalRoute installed an address.
func (c *Client) HasLocalRoute() bool {
	c.router.mu.Lock()
	defer c.router.mu.Unlock()
	return c.router.local != nil
}

// LocalAddress returns the local dial target (host:port), or "" if none.
func (c *Client) LocalAddress() string {
	c.router.mu.Lock()
	defer c.router.mu.Unlock()
	return c.router.addr
}

// Route reports which route requests currently take and, for RoutePublic with
// a local route configured, why the local one is not in use.
func (c *Client) Route() (route, reason string) { return c.router.state() }

// RouteDown is signalled once per up→down flip of the local route, so a
// prober can re-check straight away rather than on its next tick.
func (c *Client) RouteDown() <-chan struct{} { return c.router.downCh }

// HTTPClient exposes the routed http.Client for the notify_push websocket
// handshake (websocket.DialOptions.HTTPClient): same route, same session
// cookies as sync traffic.
func (c *Client) HTTPClient() *http.Client { return c.hc }

// ProbeLocal sends one PROPFIND for the DAV root's oc:id over the local
// transport and brings the route up only if the id matches the one captured at
// setup. Bounded by probeTimeout. Any failure leaves the route down with a
// reason; a wrong id is "different server", never "unreachable". If the
// CALLER's context ends the probe (shutdown, reconfiguration mid-probe), the
// route is left exactly as it was — that is the caller giving up, not the
// local endpoint failing, and must not flip an up route down.
func (c *Client) ProbeLocal(ctx context.Context) error {
	c.router.mu.Lock()
	want, has := c.router.rootID, c.router.local != nil
	c.router.mu.Unlock()
	if !has {
		return errors.New("no local route configured")
	}
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	id, err := c.RootID(ForceRoute(ctx, RouteLocal))
	if err != nil {
		if parent.Err() != nil {
			return err
		}
		c.router.markDown(reasonFor(err))
		return err
	}
	if id != want {
		c.router.markDown(ReasonDifferent)
		return fmt.Errorf("local address answers, but it is a different server or account (root id %s, expected %s)", id, want)
	}
	c.router.markUp()
	return nil
}
