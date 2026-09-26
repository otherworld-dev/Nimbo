package transport

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// This is the reproduction the field bug (GitHub issue #1) actually needed: a
// server that speaks real HTTP/2 and, mid-upload, sends the graceful-shutdown
// GOAWAY that a proxy reload (nginx -s reload) emits — the exact frame behind
// TerraEnd's "http2: Transport received Server's graceful shutdown GOAWAY …
// define Request.GetBody to avoid this error". A request whose body was already
// written is unretryable UNLESS it carries GetBody; the fix set GetBody on
// chunk PUTs. These tests prove the differential end to end.

// goawayServer is a minimal HTTP/2 server (driven by http2.Framer) that GOAWAYs
// the first failN connections right after the request body is fully received,
// and serves 201 on later connections. lastStreamID 0 on the GOAWAY marks the
// in-flight stream as unprocessed, which the client treats as retryable.
type goawayServer struct {
	ln      net.Listener
	tlsConf *tls.Config
	failN   int32
	conns   atomic.Int32
	mu      sync.Mutex
	body    []byte
}

func (s *goawayServer) serve() {
	for {
		raw, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(raw)
	}
}

func (s *goawayServer) handle(raw net.Conn) {
	defer raw.Close()
	tc := tls.Server(raw, s.tlsConf)
	if err := tc.Handshake(); err != nil {
		return
	}
	fail := s.conns.Add(1) <= s.failN

	pre := make([]byte, len(http2.ClientPreface))
	if _, err := io.ReadFull(tc, pre); err != nil {
		return
	}
	fr := http2.NewFramer(tc, tc)
	fr.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	if err := fr.WriteSettings(); err != nil {
		return
	}
	var body bytes.Buffer
	end := func(streamID uint32) {
		if fail {
			_ = fr.WriteGoAway(0, http2.ErrCodeNo, nil) // graceful shutdown; stream unprocessed
			return
		}
		s.mu.Lock()
		s.body = append([]byte(nil), body.Bytes()...)
		s.mu.Unlock()
		var hb bytes.Buffer
		enc := hpack.NewEncoder(&hb)
		_ = enc.WriteField(hpack.HeaderField{Name: ":status", Value: "201"})
		_ = enc.WriteField(hpack.HeaderField{Name: "oc-etag", Value: `"etag-goaway"`})
		_ = enc.WriteField(hpack.HeaderField{Name: "oc-fileid", Value: "fid-goaway"})
		_ = fr.WriteHeaders(http2.HeadersFrameParam{
			StreamID: streamID, BlockFragment: hb.Bytes(), EndStream: true, EndHeaders: true,
		})
	}
	for {
		f, err := fr.ReadFrame()
		if err != nil {
			return
		}
		switch f := f.(type) {
		case *http2.SettingsFrame:
			if !f.IsAck() {
				_ = fr.WriteSettingsAck()
			}
		case *http2.PingFrame:
			if !f.IsAck() {
				_ = fr.WritePing(true, f.Data)
			}
		case *http2.MetaHeadersFrame:
			if f.StreamEnded() {
				end(f.StreamID)
				hangUp(tc, fr)
				return
			}
		case *http2.DataFrame:
			body.Write(f.Data())
			if n := len(f.Data()); n > 0 {
				_ = fr.WriteWindowUpdate(0, uint32(n))
			}
			if f.StreamEnded() {
				end(f.StreamID)
				hangUp(tc, fr)
				return
			}
		}
	}
}

// hangUp ends a connection the way a real server does: close_notify, then read
// until the client closes its side. Closing the socket at once, with the
// client's WINDOW_UPDATE or SETTINGS ack still unread, makes Windows reset the
// connection, and the reset can overtake the last frame written (the GOAWAY or
// the 201), failing the client with "connection aborted/forcibly closed"
// instead (~1 run in 30 locally, reliably on a CI runner).
func hangUp(tc *tls.Conn, fr *http2.Framer) {
	_ = tc.CloseWrite()
	_ = tc.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		if _, err := fr.ReadFrame(); err != nil {
			return
		}
	}
}

func (s *goawayServer) gotBody() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.body)
}

// startGoawayServer stands one up on 127.0.0.1 and returns it with a client
// that trusts its self-signed cert and negotiates HTTP/2.
func startGoawayServer(t *testing.T, failN int32) (*goawayServer, *Client) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &goawayServer{
		ln:    ln,
		failN: failN,
		tlsConf: &tls.Config{
			Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: priv}},
			NextProtos:   []string{"h2"},
		},
	}
	go s.serve()
	t.Cleanup(func() { ln.Close() })

	c := New("https://"+ln.Addr().String(), "u", "p")
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}
	if err := http2.ConfigureTransport(tr); err != nil {
		t.Fatal(err)
	}
	c.hc = &http.Client{Transport: tr}
	return s, c
}

// The fixed chunk PUT carries GetBody, so a graceful-shutdown GOAWAY landing
// after the body was written is transparently retried on a fresh connection —
// the upload completes, bytes intact. This is issue #1's core failure, proven
// against a genuine HTTP/2 GOAWAY rather than a simulated network drop.
func TestChunkPutSurvivesRealGOAWAY(t *testing.T) {
	s, c := startGoawayServer(t, 1) // GOAWAY the first connection, serve the retry
	payload := "chunk-bytes-0123456789-abcdefghij"
	newBody := func() (io.Reader, error) { return strings.NewReader(payload), nil }

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := c.PutChunk(ctx, "up1", "00001", newBody, int64(len(payload)), "docs/big.bin"); err != nil {
		t.Fatalf("PutChunk did not survive a real graceful-shutdown GOAWAY: %v", err)
	}
	if s.conns.Load() < 2 {
		t.Fatalf("expected a retry on a second connection, saw %d connection(s)", s.conns.Load())
	}
	if s.gotBody() != payload {
		t.Fatalf("server received %q after the GOAWAY retry, want the full chunk %q", s.gotBody(), payload)
	}
}

// Negative control: the SAME GOAWAY against a request with no GetBody is
// unretryable — it reproduces TerraEnd's exact error. This proves the harness
// genuinely elicits the field condition, so the test above passes for the
// right reason.
func TestPutWithoutGetBodyFailsOnRealGOAWAY(t *testing.T) {
	s, c := startGoawayServer(t, 1)
	req, err := http.NewRequest(http.MethodPut, c.server+"/remote.php/dav/uploads/u/up1/00001", strings.NewReader("payload-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	req.GetBody = nil // the pre-fix state: an unrewindable body
	req.ContentLength = int64(len("payload-bytes"))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resp, err := c.hc.Do(req.WithContext(ctx))
	if err == nil {
		resp.Body.Close()
		t.Fatal("a GOAWAY after the body was written should have been unretryable without GetBody, but the PUT succeeded")
	}
	t.Logf("reproduced the field error: %v", err)
	if !strings.Contains(strings.ToLower(err.Error()), "goaway") {
		t.Fatalf("error was not the graceful-shutdown GOAWAY condition: %v", err)
	}
	if s.conns.Load() != 1 {
		t.Fatalf("no-GetBody request should not have retried, saw %d connection(s)", s.conns.Load())
	}
}
