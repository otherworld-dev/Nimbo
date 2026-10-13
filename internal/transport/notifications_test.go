package transport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// A notification action is only ever sent to the account's own server, since
// the request carries the login (GitHub #20). A WEB action is never sent at
// all: the UI opens it in the browser.
func TestExecuteActionStaysOnServer(t *testing.T) {
	var own, other atomic.Int32
	var gotAuth atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		own.Add(1)
		_, _, ok := r.BasicAuth()
		gotAuth.Store(ok)
	}))
	defer srv.Close()
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		other.Add(1)
	}))
	defer elsewhere.Close()

	c := New(srv.URL, "alice", "pw")
	ctx := context.Background()

	// The suspicious-login warning's button, pointed at another host.
	err := c.ExecuteAction(ctx, NotificationAction{Label: "Open", Link: elsewhere.URL + "/lookup", Type: "WEB"})
	if err == nil {
		t.Fatal("WEB action was sent")
	}
	// The same link as a request type is still refused: wrong host.
	err = c.ExecuteAction(ctx, NotificationAction{Label: "Accept", Link: elsewhere.URL + "/accept", Type: "POST"})
	if err == nil || !strings.Contains(err.Error(), "not this account's server") {
		t.Fatalf("off-server action: err = %v", err)
	}
	if n := other.Load(); n != 0 {
		t.Fatalf("other host got %d requests", n)
	}

	// Absolute and server-relative links to the account's server still work.
	if err := c.ExecuteAction(ctx, NotificationAction{Link: srv.URL + "/ocs/v2.php/apps/x", Type: "POST"}); err != nil {
		t.Fatalf("absolute own-server action: %v", err)
	}
	if err := c.ExecuteAction(ctx, NotificationAction{Link: "/ocs/v2.php/apps/x", Type: "DELETE"}); err != nil {
		t.Fatalf("relative own-server action: %v", err)
	}
	if n := own.Load(); n != 2 || !gotAuth.Load() {
		t.Fatalf("own server: %d requests, auth %v; want 2 with auth", n, gotAuth.Load())
	}
}

func TestSameServer(t *testing.T) {
	c := New("https://cloud.example.com/nextcloud", "alice", "pw")
	for target, want := range map[string]bool{
		"https://cloud.example.com/nextcloud/ocs/x": true,
		"https://CLOUD.example.com/ocs/x":           true,
		"http://cloud.example.com/ocs/x":            false, // scheme downgrade
		"https://cloud.example.com:8443/ocs/x":      false,
		"https://cloud.example.com.evil.net/ocs/x":  false,
		"https://iplookup.flagfox.net/?ip=1.2.3.4":  false,
		"https://evil.net/@cloud.example.com":       false,
	} {
		if got := c.sameServer(target); got != want {
			t.Errorf("sameServer(%q) = %v, want %v", target, got, want)
		}
	}
}

// An app password a sign-in produced but the caller isn't keeping is revoked
// with the password itself, on the account's server.
func TestDeleteAppPassword(t *testing.T) {
	var method, path, pass string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		_, pass, _ = r.BasicAuth()
	}))
	defer srv.Close()
	if err := New(srv.URL, "alice", "unused-pw").DeleteAppPassword(context.Background()); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodDelete || path != "/ocs/v2.php/core/apppassword" || pass != "unused-pw" {
		t.Fatalf("sent %s %s with password %q", method, path, pass)
	}
}
