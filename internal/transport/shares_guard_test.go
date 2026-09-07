package transport

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// countingServer records how many requests actually reached it. A guard that
// only *looks* like it refuses — but still sends the request — is no guard.
func countingServer(t *testing.T) (*httptest.Server, *int) {
	t.Helper()
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ocs":{"meta":{"status":"ok","statuscode":200},"data":{}}}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

// TestCreateShareRefusesTheAccountRoot: "" and "/" resolve to the whole
// account. Publishing every file a user owns behind one link is not something
// a mistyped path should be able to do.
func TestCreateShareRefusesTheAccountRoot(t *testing.T) {
	srv, calls := countingServer(t)
	c := New(srv.URL, "alice", "pw")

	for _, p := range []string{"", "/", "   ", "///"} {
		if _, err := c.CreatePublicLink(context.Background(), p, PublicLinkOptions{}); err == nil {
			t.Errorf("CreatePublicLink(%q) = nil error, want a refusal", p)
		}
		if _, err := c.CreateUserShare(context.Background(), p, "bob", PermRead); err == nil {
			t.Errorf("CreateUserShare(%q) = nil error, want a refusal", p)
		}
	}
	if *calls != 0 {
		t.Errorf("%d requests reached the server; a refused share must not be sent", *calls)
	}
}

// TestCreateUserShareRequiresARecipient: an empty shareWith is accepted by
// some server versions as a share with nobody, which then sits in the user's
// share list doing nothing anyone can explain.
func TestCreateUserShareRequiresARecipient(t *testing.T) {
	srv, calls := countingServer(t)
	c := New(srv.URL, "alice", "pw")

	for _, u := range []string{"", "   "} {
		if _, err := c.CreateUserShare(context.Background(), "Documents/report.pdf", u, PermRead); err == nil {
			t.Errorf("CreateUserShare(user=%q) = nil error, want a refusal", u)
		}
	}
	if *calls != 0 {
		t.Errorf("%d requests reached the server; a share with no recipient must not be sent", *calls)
	}
}

// TestDeleteShareRequiresAnID is the dangerous one: an empty id builds the URL
// ".../shares/" — a DELETE against the shares collection rather than one share.
func TestDeleteShareRequiresAnID(t *testing.T) {
	srv, calls := countingServer(t)
	c := New(srv.URL, "alice", "pw")

	for _, id := range []string{"", "   ", "/"} {
		if err := c.DeleteShare(context.Background(), id); err == nil {
			t.Errorf("DeleteShare(%q) = nil error, want a refusal", id)
		}
	}
	if *calls != 0 {
		t.Errorf("%d requests reached the server; a DELETE with no share id must never be sent", *calls)
	}
}

// TestCreatePublicLinkSendsItsOptions pins that password and expiry actually
// reach the server — a password silently dropped would leave a link the user
// believes is protected wide open.
func TestCreatePublicLinkSendsItsOptions(t *testing.T) {
	var form string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		form = string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ocs":{"meta":{"status":"ok","statuscode":200},"data":{"id":"7","url":"https://x/s/tok"}}}`)
	}))
	defer srv.Close()

	c := New(srv.URL, "alice", "pw")
	sh, err := c.CreatePublicLink(context.Background(), "Documents/report.pdf", PublicLinkOptions{
		Password:   "hunter2",
		Expiration: "2026-12-31",
	})
	if err != nil {
		t.Fatalf("CreatePublicLink: %v", err)
	}
	for _, want := range []string{"password=hunter2", "expireDate=2026-12-31", "shareType=3"} {
		if !contains(form, want) {
			t.Errorf("form %q missing %q", form, want)
		}
	}
	if sh.URL != "https://x/s/tok" {
		t.Errorf("returned share URL = %q, want the link the server issued", sh.URL)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
