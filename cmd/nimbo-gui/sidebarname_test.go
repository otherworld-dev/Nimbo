package main

import (
	"strings"
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
		{"a chosen name wins", account.Account{ID: "a", LoginName: "adam", Label: "Work (ServerA)"}, []account.Account{adam, adam2}, "Work (ServerA)"},
	} {
		if got := sidebarLabel(tc.acc, tc.all); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestCleanSidebarName(t *testing.T) {
	for in, want := range map[string]string{
		"":                  "",
		"   ":               "",
		"  Work files  ":    "Work files",
		"tincanfury\t(B)\n": "tincanfury(B)",
		"José (Ørsted)":     "José (Ørsted)",
	} {
		got, err := cleanSidebarName(in)
		if err != nil || got != want {
			t.Errorf("cleanSidebarName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := cleanSidebarName(strings.Repeat("x", maxSidebarName+1)); err == nil {
		t.Error("an over-long name was accepted")
	}
	if got, err := cleanSidebarName(strings.Repeat("é", maxSidebarName)); err != nil || got == "" {
		t.Errorf("a name at the limit (counted in characters, not bytes) was refused: %v", err)
	}
}
