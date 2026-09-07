package transport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// Nextcloud treats every cookieless basic-auth request as a fresh login, and a
// login writes a row to suspicious_login's oc_login_address table — 17.5M rows
// in seven weeks on one account (Deck #599). Keeping the session cookie the
// server hands back means the second and every later request rides the
// session and no login event fires, which is exactly what the official client
// does.
func TestClientKeepsTheServerSession(t *testing.T) {
	var logins atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("ocsession"); err != nil {
			logins.Add(1) // no session presented: the server would log in again
			http.SetCookie(w, &http.Cookie{Name: "ocsession", Value: "abc", Path: "/", HttpOnly: true})
			// Nextcloud pairs the session with its same-site CSRF cookies and
			// 503s any later request that presents one without the others.
			http.SetCookie(w, &http.Cookie{Name: "nc_sameSiteCookiestrict", Value: "true", Path: "/", HttpOnly: true})
			http.SetCookie(w, &http.Cookie{Name: "nc_sameSiteCookielax", Value: "true", Path: "/", HttpOnly: true})
		} else if c, _ := r.Cookie("nc_sameSiteCookiestrict"); c == nil || c.Value != "true" {
			t.Errorf("%s %s: session cookie sent without the same-site cookies", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL, "adam", "app-password")
	ctx := context.Background()
	for i, method := range []string{"PROPFIND", "GET", "PUT"} {
		req, err := c.NewRequest(ctx, method, srv.URL+"/remote.php/dav/files/adam/", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("request %d (%s): %v", i, method, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d (%s): status %d", i, method, resp.StatusCode)
		}
		// Basic auth still rides along on every request, so an expired session
		// simply logs in again server-side; no client-side fallback needed.
		if u, _, ok := req.BasicAuth(); !ok || u != "adam" {
			t.Fatalf("request %d (%s): basic auth missing", i, method)
		}
	}
	if got := logins.Load(); got != 1 {
		t.Fatalf("server saw %d logins across 3 requests, want 1 (session not reused)", got)
	}
}

// Two Clients are two accounts (or one account re-logged-in); their sessions
// must never leak into each other.
func TestClientSessionsAreNotShared(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, _, _ := r.BasicAuth()
		if c, err := r.Cookie("ocsession"); err == nil && c.Value != u {
			t.Errorf("user %q presented %q's session", u, c.Value)
		}
		http.SetCookie(w, &http.Cookie{Name: "ocsession", Value: u, Path: "/"})
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx := context.Background()
	for _, user := range []string{"alice", "bob"} {
		c := New(srv.URL, user, "pw")
		for range 2 {
			req, _ := c.NewRequest(ctx, "GET", srv.URL+"/status.php", nil)
			resp, err := c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
		}
	}
}

// RequestCounts is the "what is Nimbo actually sending" answer the logs could
// not give during #599: a running per-method tally covering every send path
// (retrying Do and single-shot DoOnce alike), including retried attempts.
func TestRequestCountsCoverEverySendPath(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway) // first PROPFIND is retried
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL, "adam", "pw")
	ctx := context.Background()
	req, _ := c.NewRequest(ctx, "PROPFIND", srv.URL+"/a", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	req, _ = c.NewRequest(ctx, "PUT", srv.URL+"/b", nil)
	resp, err = c.DoOnce(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	counts := c.RequestCounts()
	if counts["PROPFIND"] != 2 || counts["PUT"] != 1 {
		t.Fatalf("RequestCounts = %v, want PROPFIND:2 (one retry) PUT:1", counts)
	}
	if hits.Load() != 3 {
		t.Fatalf("server saw %d requests, want 3", hits.Load())
	}
}
