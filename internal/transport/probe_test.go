package transport

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func TestProbeTLSVerdicts(t *testing.T) {
	cert := makeCert(t, "cloud.example.com")
	var rec recorder
	srv := newTLSServer(t, certPtr(cert), &rec, davHandler(testRootID, &rec))
	addr := srv.Listener.Addr().String()

	// Untrusted: nothing trusts a fresh self-signed cert. Details are still filled.
	p, err := ProbeTLS(ctxT(t), addr, "cloud.example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Verified || p.Pinned || p.PlainHTTP || p.VerifyError == "" {
		t.Errorf("untrusted verdict wrong: %+v", p)
	}
	if p.Fingerprint != Fingerprint(cert.leaf) || p.Subject == "" || p.NotAfter.IsZero() {
		t.Errorf("certificate details missing: %+v", p)
	}
	if rec.hits.Load() != 0 {
		t.Fatalf("a bare handshake must send no request (hits=%d)", rec.hits.Load())
	}
	if rec.lastSNI() != "cloud.example.com" {
		t.Errorf("SNI = %q", rec.lastSNI())
	}

	// Pinned.
	p, _ = ProbeTLS(ctxT(t), addr, "cloud.example.com", Fingerprint(cert.leaf))
	if !p.Pinned || p.Verified {
		t.Errorf("pinned verdict wrong: %+v", p)
	}
	// The pin compares case-insensitively (users paste uppercase from browsers).
	p, _ = ProbeTLS(ctxT(t), addr, "cloud.example.com", strings.ToUpper(Fingerprint(cert.leaf)))
	if !p.Pinned {
		t.Errorf("uppercase pin must match: %+v", p)
	}
	p, _ = ProbeTLS(ctxT(t), addr, "cloud.example.com", strings.Repeat("0", 64))
	if p.Pinned {
		t.Errorf("a different pin must not match: %+v", p)
	}

	// Verified (trust store stands in for Windows).
	trustLocally(t, cert)
	p, _ = ProbeTLS(ctxT(t), addr, "cloud.example.com", "")
	if !p.Verified || p.VerifyError != "" {
		t.Errorf("verified verdict wrong: %+v", p)
	}
	// Verified is about the NAME too: the same cert for another name fails.
	p, _ = ProbeTLS(ctxT(t), addr, "other.example.com", "")
	if p.Verified {
		t.Errorf("wrong name must not verify: %+v", p)
	}
}

// nextcloudStatus is what a real status.php returns (shape confirmed on
// Nextcloud 33): the only fields ProbeNextcloud reads are installed,
// productname and versionstring.
const nextcloudStatus = `{"installed":true,"maintenance":false,"needsDbUpgrade":false,"version":"33.0.0.5","versionstring":"33.0.0","edition":"","productname":"Nextcloud","extendedSupport":false}`

// statusServer serves body with code at exactly base+"/status.php" (404
// elsewhere), recording hits, credentials, Host and the path asked for.
func statusServer(t *testing.T, cert testCert, rec *recorder, base string, code int, body string) (*httptest.Server, *atomic.Value) {
	t.Helper()
	var path atomic.Value
	srv := newTLSServer(t, certPtr(cert), rec, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.hits.Add(1)
		rec.host.Store(r.Host)
		path.Store(r.URL.Path)
		if r.Header.Get("Authorization") != "" {
			rec.sawAuth.Store(true)
		}
		if r.URL.Path != base+"/status.php" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(code)
		io.WriteString(w, body)
	}))
	return srv, &path
}

// ProbeNextcloud is the wrong-host guard that runs BEFORE the trust dialog:
// it must be shaped like a real local-route request (so a subpath install
// and a trusted_domains check both pass), send no credentials (the
// certificate is not trusted yet), and refuse to talk to anything but the
// leaf the handshake just showed.
func TestProbeNextcloud(t *testing.T) {
	cert := makeCert(t, "cloud.example.com")
	pin := Fingerprint(cert.leaf)

	t.Run("nextcloud answers, unauthenticated, routed like the real thing", func(t *testing.T) {
		var rec recorder
		srv, path := statusServer(t, cert, &rec, "/nextcloud", http.StatusOK, nextcloudStatus)
		p, err := ProbeNextcloud(ctxT(t), "https://cloud.example.com/nextcloud/", srv.Listener.Addr().String(), pin)
		if err != nil {
			t.Fatal(err)
		}
		if !p.Nextcloud || p.Product != "Nextcloud" || p.Version != "33.0.0" {
			t.Errorf("verdict wrong: %+v", p)
		}
		if rec.sawAuth.Load() {
			t.Error("status.php must be fetched without credentials — the certificate isn't trusted yet")
		}
		if rec.lastHost() != "cloud.example.com" || rec.lastSNI() != "cloud.example.com" {
			t.Errorf("Host/SNI must be the public host: host=%q sni=%q", rec.lastHost(), rec.lastSNI())
		}
		if got, _ := path.Load().(string); got != "/nextcloud/status.php" {
			t.Errorf("path = %q, want the public URL's path + /status.php", got)
		}
	})

	t.Run("something else answers", func(t *testing.T) {
		var rec recorder
		srv, _ := statusServer(t, cert, &rec, "", http.StatusOK, "<html><title>Router login</title></html>")
		p, err := ProbeNextcloud(ctxT(t), "https://cloud.example.com", srv.Listener.Addr().String(), pin)
		if err != nil {
			t.Fatal(err)
		}
		if p.Nextcloud || p.Detail == "" {
			t.Errorf("an HTML page must not pass: %+v", p)
		}

		srv404, _ := statusServer(t, cert, &rec, "/elsewhere", http.StatusOK, nextcloudStatus)
		p, err = ProbeNextcloud(ctxT(t), "https://cloud.example.com", srv404.Listener.Addr().String(), pin)
		if err != nil {
			t.Fatal(err)
		}
		if p.Nextcloud || !strings.Contains(p.Detail, "404") {
			t.Errorf("a 404 must not pass and must say so: %+v", p)
		}
	})

	t.Run("redirects are not followed", func(t *testing.T) {
		var rec recorder
		srv := newTLSServer(t, certPtr(cert), &rec, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec.hits.Add(1)
			http.Redirect(w, r, "https://somewhere.else/status.php", http.StatusFound)
		}))
		p, err := ProbeNextcloud(ctxT(t), "https://cloud.example.com", srv.Listener.Addr().String(), pin)
		if err != nil {
			t.Fatal(err)
		}
		if p.Nextcloud || rec.hits.Load() != 1 {
			t.Errorf("a redirect must count as no answer and not be followed: %+v hits=%d", p, rec.hits.Load())
		}
	})

	t.Run("another certificate gets nothing", func(t *testing.T) {
		var rec recorder
		srv, _ := statusServer(t, cert, &rec, "", http.StatusOK, nextcloudStatus)
		_, err := ProbeNextcloud(ctxT(t), "https://cloud.example.com", srv.Listener.Addr().String(), strings.Repeat("0", 64))
		var pm *PinMismatchError
		if !errors.As(err, &pm) {
			t.Fatalf("want a pin mismatch, got %v", err)
		}
		if rec.hits.Load() != 0 {
			t.Fatalf("the request must never leave the client on a pin mismatch (hits=%d)", rec.hits.Load())
		}
	})
}

// Live shape check, opt-in: NIMBO_LIVE_STATUS=https://your.server[/path]
// fetches the real status.php pinned to the certificate the handshake shows,
// so the JSON parsing is proven against a real Nextcloud, not just the fixture.
func TestProbeNextcloudLive(t *testing.T) {
	server := os.Getenv("NIMBO_LIVE_STATUS")
	if server == "" {
		t.Skip("set NIMBO_LIVE_STATUS=https://host to probe a real server")
	}
	u, err := url.Parse(server)
	if err != nil || u.Scheme != "https" {
		t.Fatalf("NIMBO_LIVE_STATUS must be an https URL: %v", err)
	}
	addr := u.Host
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr += ":443"
	}
	tp, err := ProbeTLS(ctxT(t), addr, u.Hostname(), "")
	if err != nil {
		t.Fatal(err)
	}
	p, err := ProbeNextcloud(ctxT(t), server, addr, tp.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s answers as %q %q (nextcloud=%v detail=%q)", server, p.Product, p.Version, p.Nextcloud, p.Detail)
	if !p.Nextcloud {
		t.Fatalf("expected a Nextcloud: %+v", p)
	}
}

func TestProbeTLSPlainHTTPAndUnreachable(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer plain.Close()
	p, err := ProbeTLS(ctxT(t), plain.Listener.Addr().String(), "cloud.example.com", "")
	if err != nil || !p.PlainHTTP {
		t.Errorf("plain HTTP endpoint: %+v, %v", p, err)
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	addr := closed.Listener.Addr().String()
	closed.Close()
	if _, err := ProbeTLS(ctxT(t), addr, "cloud.example.com", ""); err == nil {
		t.Error("closed port must return an error")
	}
}
