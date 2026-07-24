package account

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNormalizeServerURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"cloud.example.com", "https://cloud.example.com"},
		{" cloud.example.com ", "https://cloud.example.com"},
		{"https://cloud.example.com/", "https://cloud.example.com"},
		{"http://cloud.example.com", "http://cloud.example.com"},
		{"cloud.example.com//", "https://cloud.example.com"},
		{"https://cloud.example.com/nextcloud/", "https://cloud.example.com/nextcloud"},
	}
	for _, c := range cases {
		if got := normalizeServerURL(c.in); got != c.want {
			t.Errorf("normalizeServerURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A bare host must get an https:// scheme: against a plain-HTTP test server the
// request then fails with the distinctive "HTTP response to HTTPS client"
// error, proving the scheme was prepended (today it fails URL parsing instead).
func TestInitLoginDefaultsToHTTPS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	_, err := InitLogin(context.Background(), host)
	if err == nil || !strings.Contains(err.Error(), "HTTPS client") {
		t.Fatalf("want https-vs-http transport error proving scheme default, got: %v", err)
	}
}

func loginPollHandler(t *testing.T, calls *atomic.Int32, transientFirst bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			if transientFirst {
				// Abruptly drop the connection: the client sees a transport
				// error (EOF), the kind a Wi-Fi→cellular handover produces.
				hj, ok := w.(http.Hijacker)
				if !ok {
					t.Fatal("test server does not support hijacking")
				}
				conn, _, err := hj.Hijack()
				if err != nil {
					t.Fatalf("hijack: %v", err)
				}
				conn.Close()
				return
			}
			w.WriteHeader(http.StatusNotFound)
		case 2:
			w.WriteHeader(http.StatusNotFound) // still pending
		default:
			_ = json.NewEncoder(w).Encode(map[string]string{
				"server":      "https://cloud.example.com",
				"loginName":   "adam",
				"appPassword": "s3cret",
			})
		}
	}
}

func TestFlowPollRetriesTransientErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(loginPollHandler(t, &calls, true))
	defer srv.Close()

	f := &Flow{pollToken: "tok", pollEndpoint: srv.URL, hc: srv.Client(), pollInterval: 10 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	creds, err := f.Poll(ctx)
	if err != nil {
		t.Fatalf("Poll should survive a single transport error, got: %v", err)
	}
	if creds.AppPassword != "s3cret" || creds.LoginName != "adam" {
		t.Fatalf("unexpected creds: %+v", creds)
	}
	if n := calls.Load(); n < 3 {
		t.Fatalf("expected >=3 poll attempts, got %d", n)
	}
}

func TestFlowPollStopsOnTerminalServerError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	f := &Flow{pollToken: "tok", pollEndpoint: srv.URL, hc: srv.Client(), pollInterval: 10 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := f.Poll(ctx); err == nil || !strings.Contains(err.Error(), "server returned") {
		t.Fatalf("want terminal server error, got: %v", err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("terminal error must not be retried, got %d attempts", n)
	}
}

func TestFlowPollHonoursCancelDuringTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, _ := w.(http.Hijacker)
		conn, _, err := hj.Hijack()
		if err == nil {
			conn.Close()
		}
	}))
	defer srv.Close()

	f := &Flow{pollToken: "tok", pollEndpoint: srv.URL, hc: srv.Client(), pollInterval: 10 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if _, err := f.Poll(ctx); err == nil {
		t.Fatal("cancelled Poll must return an error")
	}
}
