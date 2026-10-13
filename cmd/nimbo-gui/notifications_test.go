package main

import "testing"

// Only an absolute http(s) link from a notification goes to the shell to open
// (GitHub #20): never a file:, a custom protocol or something unparseable.
func TestIsWebLink(t *testing.T) {
	for href, want := range map[string]bool{
		"https://iplookup.flagfox.net/?ip=1.2.3.4": true,
		"http://cloud.example.com/apps/files/":     true,
		"/apps/files/":                             false,
		"file:///C:/Windows/System32/calc.exe":     false,
		"ms-settings:privacy":                      false,
		"javascript:alert(1)":                      false,
		"":                                         false,
	} {
		if got := isWebLink(href); got != want {
			t.Errorf("isWebLink(%q) = %v, want %v", href, got, want)
		}
	}
}
