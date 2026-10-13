package main

import (
	"net/url"
	"testing"

	"github.com/otherworld/nimbo/internal/account"
)

// The account shown in place of one whose sign-in was turned down is the first
// that can still sign in: not turned down itself and still holding a password.
// None left is the signed-out state, which is what stops a launch with every
// password revoked from switching between accounts forever (GitHub #20).
func TestPickFallback(t *testing.T) {
	accts := []account.Account{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	lost := map[string]bool{"a": true}
	secret := map[string]bool{"a": true, "c": true}
	got, ok := pickFallback(accts, func(id string) bool { return lost[id] }, func(id string) bool { return secret[id] })
	if !ok || got.ID != "c" {
		t.Fatalf("picked %q (%v), want c: a was turned down and b has no password", got.ID, ok)
	}
	lost["c"] = true
	if got, ok := pickFallback(accts, func(id string) bool { return lost[id] }, func(id string) bool { return secret[id] }); ok {
		t.Fatalf("picked %q with no account able to sign in", got.ID)
	}
}

// A sign-in toast names the account to sign in again, and an ID survives the
// activation string whatever it holds.
func TestSignInArgs(t *testing.T) {
	v, err := url.ParseQuery(signInArgs("ab&cd=1"))
	if err != nil {
		t.Fatal(err)
	}
	if v.Get("action") != "login" || v.Get("acct") != "ab&cd=1" {
		t.Fatalf("parsed %v", v)
	}
}

// The browser decides who signs in, so a window asking for one account can get
// a grant for another (GitHub #20 testing: Edge was still logged in as test
// while the window asked for Test Account 2). That grant is refused unless the
// window was asking for any account.
func TestWrongAccount(t *testing.T) {
	ta2 := account.Account{ID: "ta2"}
	test := account.Account{ID: "test"}
	for _, c := range []struct {
		name  string
		want  string
		got   account.Account
		known bool
		wrong bool
	}{
		{"the account asked for", "ta2", ta2, true, false},
		{"another account Nimbo has", "ta2", test, true, true},
		{"an account Nimbo doesn't have", "ta2", account.Account{}, false, true},
		{"any account, a known one", "", test, true, false},
		{"any account, a new one", "", account.Account{}, false, false},
	} {
		if got := wrongAccount(c.want, c.got, c.known); got != c.wrong {
			t.Errorf("%s: wrongAccount = %v, want %v", c.name, got, c.wrong)
		}
	}
}
