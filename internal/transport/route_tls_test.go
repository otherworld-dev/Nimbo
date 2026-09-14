package transport

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testRootID = "00000042ocabc123"

type testCert struct {
	tlsCert tls.Certificate
	leaf    *x509.Certificate
}

// makeCert is a self-signed certificate valid for name (DNS SAN) — the shape a
// user's own LAN certificate has.
func makeCert(t *testing.T, name string) testCert {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{name},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return testCert{tlsCert: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}, leaf: leaf}
}

// recorder captures what a test server saw.
type recorder struct {
	hits    atomic.Int32
	sawAuth atomic.Bool
	host    atomic.Value // string: last Host header
	sni     atomic.Value // string: last SNI
}

func (r *recorder) lastHost() string { v, _ := r.host.Load().(string); return v }
func (r *recorder) lastSNI() string  { v, _ := r.sni.Load().(string); return v }

// davHandler answers every PROPFIND with a root-id multistatus and everything
// else with 200, recording hits and whether credentials arrived.
func davHandler(rootID string, rec *recorder) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.hits.Add(1)
		rec.host.Store(r.Host)
		if r.Header.Get("Authorization") != "" {
			rec.sawAuth.Store(true)
		}
		if r.Method == "PROPFIND" {
			w.WriteHeader(http.StatusMultiStatus)
			fmt.Fprintf(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns"><d:response><d:href>%s</d:href><d:propstat><d:prop><oc:id>%s</oc:id></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`, r.URL.Path, rootID)
			return
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok")
	})
}

// newTLSServer serves h over TLS with cert, recording SNI into rec. certs is a
// pointer so a test can swap the served certificate mid-run.
func newTLSServer(t *testing.T, certs *atomic.Pointer[tls.Certificate], rec *recorder, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{
		GetCertificate: func(hi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			rec.sni.Store(hi.ServerName)
			return certs.Load(), nil
		},
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func certPtr(c testCert) *atomic.Pointer[tls.Certificate] {
	p := &atomic.Pointer[tls.Certificate]{}
	p.Store(&c.tlsCert)
	return p
}

// newRoutedTestClient builds a Client for https://cloud.example.com whose
// PUBLIC transport dials publicSrv (the test can't resolve the name), trusting
// pubCert, and whose local route is localAddr.
func newRoutedTestClient(t *testing.T, publicSrv *httptest.Server, pubCert testCert, localAddr, pin, rootID string) *Client {
	t.Helper()
	c := New("https://cloud.example.com", "alice", "pw")
	pool := x509.NewCertPool()
	pool.AddCert(pubCert.leaf)
	c.router.public = &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, publicSrv.Listener.Addr().String())
		},
		TLSClientConfig: &tls.Config{RootCAs: pool},
	}
	if err := c.SetLocalRoute(localAddr, pin, rootID); err != nil {
		t.Fatal(err)
	}
	return c
}

// trustLocally makes verified mode accept cert (stands in for the OS store).
func trustLocally(t *testing.T, cert testCert) {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(cert.leaf)
	localRootCAs = pool
	t.Cleanup(func() { localRootCAs = nil })
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestLocalRouteUsedWhenUp(t *testing.T) {
	good := makeCert(t, "cloud.example.com")
	trustLocally(t, good)
	var pubRec, locRec recorder
	pub := newTLSServer(t, certPtr(good), &pubRec, davHandler(testRootID, &pubRec))
	loc := newTLSServer(t, certPtr(good), &locRec, davHandler(testRootID, &locRec))
	c := newRoutedTestClient(t, pub, good, loc.Listener.Addr().String(), "", testRootID)

	if err := c.ProbeLocal(ctxT(t)); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if route, _ := c.Route(); route != RouteLocal {
		t.Fatalf("route = %q", route)
	}
	if id, err := c.RootID(ctxT(t)); err != nil || id != testRootID {
		t.Fatalf("RootID via local: %q, %v", id, err)
	}
	if !locRec.sawAuth.Load() {
		t.Fatal("credentials should reach the trusted local server")
	}
	if locRec.hits.Load() != 2 || pubRec.hits.Load() != 0 {
		t.Errorf("hits local=%d public=%d, want 2/0", locRec.hits.Load(), pubRec.hits.Load())
	}
	if locRec.lastHost() != "cloud.example.com" || locRec.lastSNI() != "cloud.example.com" {
		t.Errorf("Host=%q SNI=%q, want the public name for both", locRec.lastHost(), locRec.lastSNI())
	}
}

// Verified mode: a certificate the store doesn't trust fails the handshake, so
// the local server never sees a request (let alone credentials), and traffic
// goes public.
func TestVerifiedModeRejectsUntrustedCert(t *testing.T) {
	good, impostor := makeCert(t, "cloud.example.com"), makeCert(t, "cloud.example.com")
	trustLocally(t, good)
	var pubRec, locRec recorder
	pub := newTLSServer(t, certPtr(good), &pubRec, davHandler(testRootID, &pubRec))
	loc := newTLSServer(t, certPtr(impostor), &locRec, davHandler(testRootID, &locRec))
	c := newRoutedTestClient(t, pub, good, loc.Listener.Addr().String(), "", testRootID)

	if err := c.ProbeLocal(ctxT(t)); err == nil {
		t.Fatal("probe must fail on an untrusted certificate")
	}
	if route, reason := c.Route(); route != RoutePublic || reason != ReasonCertChanged {
		t.Errorf("Route() = %q, %q", route, reason)
	}
	if locRec.hits.Load() != 0 || locRec.sawAuth.Load() {
		t.Fatalf("impostor handled a request (hits=%d auth=%v)", locRec.hits.Load(), locRec.sawAuth.Load())
	}
	if id, err := c.RootID(ctxT(t)); err != nil || id != testRootID || pubRec.hits.Load() != 1 {
		t.Fatalf("public fallback: %q, %v, hits=%d", id, err, pubRec.hits.Load())
	}
}

// Pinned mode: the store trusts nothing here, the pin alone admits cert A; when
// the server switches to cert B the handshake fails before any request bytes.
func TestPinnedModeRejectsOtherCert(t *testing.T) {
	pubCert, a, b := makeCert(t, "cloud.example.com"), makeCert(t, "cloud.example.com"), makeCert(t, "cloud.example.com")
	var pubRec, locRec recorder
	pub := newTLSServer(t, certPtr(pubCert), &pubRec, davHandler(testRootID, &pubRec))
	served := certPtr(a)
	loc := newTLSServer(t, served, &locRec, davHandler(testRootID, &locRec))
	c := newRoutedTestClient(t, pub, pubCert, loc.Listener.Addr().String(), Fingerprint(a.leaf), testRootID)

	if err := c.ProbeLocal(ctxT(t)); err != nil {
		t.Fatalf("probe with the pinned cert: %v", err)
	}
	if route, _ := c.Route(); route != RouteLocal {
		t.Fatalf("route = %q", route)
	}
	before := locRec.hits.Load()

	served.Store(&b.tlsCert)
	c.router.local.CloseIdleConnections() // force a fresh handshake
	_, err := c.RootID(ctxT(t))
	var pm *PinMismatchError
	if err == nil || !errorsAs(err, &pm) {
		t.Fatalf("expected PinMismatchError, got %v", err)
	}
	if route, reason := c.Route(); route != RoutePublic || reason != ReasonCertChanged {
		t.Errorf("Route() = %q, %q", route, reason)
	}
	if locRec.hits.Load() != before {
		t.Fatalf("a request reached the server with the wrong certificate")
	}
	if id, err := c.RootID(ctxT(t)); err != nil || id != testRootID {
		t.Fatalf("public fallback after pin failure: %q, %v", id, err)
	}
}

// Losing the LAN mid-run: the next retried request lands on public, the
// down-signal fires, and a probe against a live listener restores local.
func TestFailoverToPublicAndRecovery(t *testing.T) {
	good := makeCert(t, "cloud.example.com")
	trustLocally(t, good)
	var pubRec, locRec recorder
	pub := newTLSServer(t, certPtr(good), &pubRec, davHandler(testRootID, &pubRec))
	loc := newTLSServer(t, certPtr(good), &locRec, davHandler(testRootID, &locRec))
	c := newRoutedTestClient(t, pub, good, loc.Listener.Addr().String(), "", testRootID)
	if err := c.ProbeLocal(ctxT(t)); err != nil {
		t.Fatal(err)
	}

	loc.Close() // the LAN goes away
	req, _ := c.NewRequest(ctxT(t), http.MethodGet, "https://cloud.example.com/status.php", nil)
	resp, err := c.Do(req) // GET is retried: attempt 1 fails locally, attempt 2 goes public
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("Do after LAN loss: %v %v", resp, err)
	}
	resp.Body.Close()
	if route, reason := c.Route(); route != RoutePublic || reason != ReasonUnreachable {
		t.Errorf("Route() = %q, %q", route, reason)
	}
	select {
	case <-c.RouteDown():
	default:
		t.Error("RouteDown must have fired")
	}
	if pubRec.hits.Load() != 1 {
		t.Errorf("public hits = %d, want 1", pubRec.hits.Load())
	}

	var locRec2 recorder
	loc2 := newTLSServer(t, certPtr(good), &locRec2, davHandler(testRootID, &locRec2))
	if err := c.SetLocalRoute(loc2.Listener.Addr().String(), "", testRootID); err != nil {
		t.Fatal(err)
	}
	if err := c.ProbeLocal(ctxT(t)); err != nil {
		t.Fatalf("recovery probe: %v", err)
	}
	if route, _ := c.Route(); route != RouteLocal {
		t.Errorf("route after recovery = %q", route)
	}
}

func TestProbeRejectsDifferentServer(t *testing.T) {
	good := makeCert(t, "cloud.example.com")
	trustLocally(t, good)
	var pubRec, locRec recorder
	pub := newTLSServer(t, certPtr(good), &pubRec, davHandler(testRootID, &pubRec))
	loc := newTLSServer(t, certPtr(good), &locRec, davHandler("00000007ocOTHER", &locRec))
	c := newRoutedTestClient(t, pub, good, loc.Listener.Addr().String(), "", testRootID)
	if err := c.ProbeLocal(ctxT(t)); err == nil {
		t.Fatal("probe must fail when oc:id differs")
	}
	if route, reason := c.Route(); route != RoutePublic || reason != ReasonDifferent {
		t.Errorf("Route() = %q, %q", route, reason)
	}
	if id, err := c.RootID(ctxT(t)); err != nil || id != testRootID {
		t.Fatalf("traffic must stay public: %q, %v", id, err)
	}
}

func errorsAs(err error, target any) bool { return errors.As(err, target) }

// Fingerprint is what the trust dialog shows and what a pin is compared
// against, so it must be exactly the SHA-256 of the leaf's DER bytes,
// lowercase hex — matching what browsers and `openssl x509 -fingerprint
// -sha256` show (minus colons).
func TestFingerprintIsSHA256OfDER(t *testing.T) {
	c := makeCert(t, "cloud.example.com")
	sum := sha256.Sum256(c.leaf.Raw)
	want := hex.EncodeToString(sum[:])
	got := Fingerprint(c.leaf)
	if got != want {
		t.Fatalf("Fingerprint = %q, want %q", got, want)
	}
	if len(got) != 64 {
		t.Fatalf("len(Fingerprint) = %d, want 64", len(got))
	}
	if strings.ToLower(got) != got {
		t.Fatalf("Fingerprint = %q, want all lowercase", got)
	}
}
