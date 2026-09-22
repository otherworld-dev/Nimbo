package account

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// Complete owns the whole sign-in write: secret first, then the account into
// the store as the new default, under the store's lock so a concurrent
// account change is not thrown away (Deck #693).
func TestCompleteRecordsAccountAsDefault(t *testing.T) {
	fake := newFakeSecretStore()
	SetSecretStore(fake)
	defer SetSecretStore(keychainStore{})
	path := filepath.Join(t.TempDir(), "accounts.json")

	got, err := Complete(path, Credentials{Server: "https://cloud.example.com/", LoginName: "alice", AppPassword: "pw"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got.ServerURL != "https://cloud.example.com" || got.LoginName != "alice" {
		t.Errorf("returned account = %+v", got)
	}
	if pw, _ := fake.Get(got.ID); pw != "pw" {
		t.Errorf("secret for %s = %q, want \"pw\"", got.ID, pw)
	}
	st, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if def, ok := st.Default(); !ok || def != got {
		t.Errorf("stored default = %+v (ok=%v), want %+v", def, ok, got)
	}
}

// If the store cannot be written the secret must not be left behind: an
// orphan keychain entry with no account to own it is invisible and never
// cleaned up.
func TestCompleteRollsBackSecretWhenStoreFails(t *testing.T) {
	fake := newFakeSecretStore()
	SetSecretStore(fake)
	defer SetSecretStore(keychainStore{})
	// A regular file where the store's directory should be: neither reading
	// nor creating accounts.json under it can succeed.
	blocker := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(blocker, "accounts.json")

	_, err := Complete(path, Credentials{Server: "https://cloud.example.com", LoginName: "alice", AppPassword: "pw"})
	if err == nil {
		t.Fatal("Complete succeeded with an unwritable store")
	}
	if len(fake.m) != 0 {
		t.Errorf("secret left behind after the store write failed: %v", fake.m)
	}
}
