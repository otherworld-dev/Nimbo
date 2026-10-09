package main

import (
	"testing"

	"github.com/otherworld/nimbo/internal/account"
	"github.com/otherworld/nimbo/internal/brand"
)

func TestSidebarLabel(t *testing.T) {
	n := brand.Current.Name
	adam := account.Account{ID: "a", ServerURL: "https://cloud.example.com", LoginName: "adam"}
	adam2 := account.Account{ID: "b", ServerURL: "https://other.example.org/nc", LoginName: "Adam"}
	work := account.Account{ID: "c", ServerURL: "https://cloud.example.com", LoginName: "work"}

	for _, tc := range []struct {
		name string
		acc  account.Account
		all  []account.Account
		want string
	}{
		{"single account still names the login", adam, []account.Account{adam}, n + " - adam"},
		{"store not loaded", adam, nil, n + " - adam"},
		{"different logins", work, []account.Account{adam, work}, n + " - work"},
		{"same login elsewhere adds the server", adam, []account.Account{adam, adam2}, n + " - adam on cloud.example.com"},
		{"and for the other one too", adam2, []account.Account{adam, adam2}, n + " - Adam on other.example.org"},
		{"no login falls back to the product", account.Account{ID: "x"}, nil, n},
	} {
		if got := sidebarLabel(tc.acc, tc.all); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
