package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/url"
	"testing"
)

func TestHostPort(t *testing.T) {
	cases := map[string]string{
		"https://cloud.example.com":         "cloud.example.com:443",
		"https://Cloud.Example.com:443/sub": "cloud.example.com:443",
		"https://cloud.example.com:8443":    "cloud.example.com:8443",
		"http://cloud.example.com":          "cloud.example.com:80",
		"https://[fd00::1]":                 "[fd00::1]:443",
		"https://[fd00::1]:8443/remote.php": "[fd00::1]:8443",
	}
	for in, want := range cases {
		if got := hostPort(in); got != want {
			t.Errorf("hostPort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPickRoutes(t *testing.T) {
	pub, loc := &http.Transport{}, &http.Transport{}
	r := newRouter(pub, "https://cloud.example.com")
	req := func(ctx context.Context, rawURL string) *http.Request {
		u, _ := url.Parse(rawURL)
		return (&http.Request{URL: u, Header: http.Header{}}).WithContext(ctx)
	}
	bg := context.Background()
	acct := "https://cloud.example.com/remote.php/dav/files/alice/"

	if tr, _ := r.pick(req(bg, acct)); tr != pub {
		t.Error("no local route configured: must pick public")
	}
	r.mu.Lock()
	r.local, r.addr = loc, "192.168.1.100:443"
	r.mu.Unlock()
	if tr, _ := r.pick(req(bg, acct)); tr != pub {
		t.Error("local route down: must pick public")
	}
	if tr, isLocal := r.pick(req(ForceRoute(bg, RouteLocal), acct)); tr != loc || !isLocal {
		t.Error("forced local while down: must pick local")
	}
	r.markUp()
	if tr, isLocal := r.pick(req(bg, acct)); tr != loc || !isLocal {
		t.Error("route up + account host: must pick local")
	}
	if tr, _ := r.pick(req(bg, "https://cloud.example.com:443/status.php")); tr != loc {
		t.Error("explicit default port is the same host")
	}
	if tr, _ := r.pick(req(bg, "https://push.example.com/push/ws")); tr != pub {
		t.Error("other host: must pick public even while up")
	}
	if tr, _ := r.pick(req(ForceRoute(bg, RoutePublic), acct)); tr != pub {
		t.Error("forced public while up: must pick public")
	}
}

func TestConnectionErrorClassification(t *testing.T) {
	timeout := &net.OpError{Op: "dial", Err: errors.New("i/o timeout")}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context canceled", context.Canceled, false},
		{"deadline", context.DeadlineExceeded, false},
		{"dial error", timeout, true},
		{"pin mismatch", &PinMismatchError{Want: "a", Got: "b"}, true},
		{"cert verification", &tls.CertificateVerificationError{Err: errors.New("x509: unknown authority")}, true},
		{"plain http answered", tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}, true},
		{"http status", &StatusError{Code: 502, Status: "502 Bad Gateway"}, false},
		{"wrapped dial error", &url.Error{Op: "Get", Err: timeout}, true},
		{"wrapped http status", &url.Error{Op: "Get", Err: &StatusError{Code: 502, Status: "502 Bad Gateway"}}, false},
		{"wrapped context canceled", &url.Error{Op: "Get", Err: context.Canceled}, false},
		{"wrapped pin mismatch", &url.Error{Op: "Get", Err: &PinMismatchError{Want: "a", Got: "b"}}, true},
	}
	for _, c := range cases {
		if got := connectionError(c.err); got != c.want {
			t.Errorf("%s: connectionError = %v, want %v", c.name, got, c.want)
		}
	}
	if reasonFor(&PinMismatchError{}) != ReasonCertChanged || reasonFor(timeout) != ReasonUnreachable {
		t.Error("reasonFor mapping wrong")
	}
	if reasonFor(&url.Error{Err: &PinMismatchError{}}) != ReasonCertChanged {
		t.Error("reasonFor must see through a *url.Error wrapper")
	}
}

func TestSetLocalRoute(t *testing.T) {
	c := New("https://cloud.example.com", "alice", "pw")
	if c.HasLocalRoute() {
		t.Fatal("fresh client must have no local route")
	}
	if route, _ := c.Route(); route != RoutePublic {
		t.Fatalf("fresh client route = %q", route)
	}
	if err := c.SetLocalRoute("192.168.1.100", "", "id"); err != nil {
		t.Fatal(err)
	}
	if !c.HasLocalRoute() || c.LocalAddress() != "192.168.1.100:443" {
		t.Errorf("port not defaulted: %q", c.LocalAddress())
	}
	if route, _ := c.Route(); route != RoutePublic {
		t.Errorf("a freshly set route must start DOWN, got %q", route)
	}
	_ = c.SetLocalRoute("[fd00::1]", "", "id")
	if c.LocalAddress() != "[fd00::1]:443" {
		t.Errorf("ipv6 port not defaulted: %q", c.LocalAddress())
	}
	_ = c.SetLocalRoute("nas.local:8443", "", "id")
	if c.LocalAddress() != "nas.local:8443" {
		t.Errorf("explicit port lost: %q", c.LocalAddress())
	}
	_ = c.SetLocalRoute("", "", "")
	if c.HasLocalRoute() {
		t.Error("empty address must remove the route")
	}
	h := New("http://cloud.example.com", "alice", "pw")
	if err := h.SetLocalRoute("192.168.1.100", "", "id"); err == nil || h.HasLocalRoute() {
		t.Error("http:// account must refuse a local route")
	}
}

func TestRouteDownSignalsOnFlipOnly(t *testing.T) {
	c := New("https://cloud.example.com", "alice", "pw")
	_ = c.SetLocalRoute("192.168.1.100", "", "id")
	c.router.markDown(ReasonUnreachable) // already down: no flip
	select {
	case <-c.RouteDown():
		t.Fatal("down→down must not signal")
	default:
	}
	c.router.markUp()
	c.router.markDown(ReasonCertChanged)
	select {
	case <-c.RouteDown():
	default:
		t.Fatal("up→down must signal")
	}
	if route, reason := c.Route(); route != RoutePublic || reason != ReasonCertChanged {
		t.Errorf("Route() = %q, %q", route, reason)
	}
	c.router.markDown(ReasonUnreachable)
	select {
	case <-c.RouteDown():
		t.Fatal("second markDown without an up must not signal again")
	default:
	}
}

// A probe cancelled by the CALLER (shutdown, reconfiguration mid-probe) must
// not flip an up route down — only the local endpoint itself failing to
// answer should do that. An already-cancelled context makes RootID fail
// before any dial, so this needs no listener.
func TestProbeLocalIgnoresCallerCancellation(t *testing.T) {
	c := New("https://cloud.example.com", "alice", "pw")
	_ = c.SetLocalRoute("192.168.1.100", "", "id")
	c.router.markUp()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.ProbeLocal(ctx); err == nil {
		t.Fatal("expected an error from an already-cancelled context")
	}
	if route, _ := c.Route(); route != RouteLocal {
		t.Errorf("caller cancellation must not flip the route down, got %q", route)
	}
	select {
	case <-c.RouteDown():
		t.Fatal("caller cancellation must not signal RouteDown")
	default:
	}
}
