package transport

import (
	"context"
	"net"
	"testing"
	"time"
)

// A server that accepts the TCP connection and then never responds must not
// hang a request forever: engine construction fetches capabilities on a
// deadline-free context (the mobile facade holds its client mutex across it),
// so the connection phases need their own bounds.
func TestSilentServerTimesOutViaResponseHeaderBound(t *testing.T) {
	origDial, origTLS, origHdr := dialTimeout, tlsHandshakeTimeout, responseHeaderTimeout
	dialTimeout, tlsHandshakeTimeout, responseHeaderTimeout = time.Second, 200*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { dialTimeout, tlsHandshakeTimeout, responseHeaderTimeout = origDial, origTLS, origHdr })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { // accept and go silent — a captive portal / half-dead server
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
		}
	}()

	c := New("http://"+ln.Addr().String(), "u", "pw")
	req, err := c.NewRequest(context.Background(), "GET", "http://"+ln.Addr().String()+"/", nil)
	if err != nil {
		t.Fatal(err)
	}

	type result struct{ err error }
	res := make(chan result, 1)
	go func() {
		resp, err := c.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		res <- result{err}
	}()
	select {
	case r := <-res:
		if r.err == nil {
			t.Fatal("request against a silent server must fail")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request hung — no connection-phase timeout is wired into the transport")
	}
}
