package main

import "testing"

// parseExternalLink guards the path from a server-controlled page to the OS
// launcher, so the cases that matter are the ones it must refuse.
func TestParseExternalLink(t *testing.T) {
	long := "https://example.com/" + string(make([]byte, maxExternalLinkLen))

	cases := []struct {
		name   string
		window string
		msg    string
		want   string
	}{
		{"http link", "app:bookmarks", externalLinkPrefix + "http://example.com/x", "http://example.com/x"},
		{"https link", "app:bookmarks", externalLinkPrefix + "https://example.com/x?a=1#f", "https://example.com/x?a=1#f"},

		{"not an app window", "flyout", externalLinkPrefix + "https://example.com/", ""},
		{"settings window", "settings", externalLinkPrefix + "https://example.com/", ""},
		{"no prefix", "app:bookmarks", "https://example.com/", ""},
		{"wrong prefix", "app:bookmarks", "nimbo:other:https://example.com/", ""},
		{"empty url", "app:bookmarks", externalLinkPrefix, ""},

		// Other schemes would let a crafted page choose the handler rather
		// than just the browser.
		{"file scheme", "app:bookmarks", externalLinkPrefix + "file:///C:/Windows/System32/calc.exe", ""},
		{"ms-settings scheme", "app:bookmarks", externalLinkPrefix + "ms-settings:windowsupdate", ""},
		{"javascript scheme", "app:bookmarks", externalLinkPrefix + "javascript:alert(1)", ""},
		{"no scheme", "app:bookmarks", externalLinkPrefix + "//example.com/x", ""},
		{"relative", "app:bookmarks", externalLinkPrefix + "/apps/files", ""},
		{"no host", "app:bookmarks", externalLinkPrefix + "http:///x", ""},

		{"control character", "app:bookmarks", externalLinkPrefix + "https://example.com/\n-x", ""},
		{"over length", "app:bookmarks", externalLinkPrefix + long, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseExternalLink(c.window, c.msg)
			if c.want == "" {
				if ok {
					t.Fatalf("parseExternalLink(%q, %q) = %q, true; want refused", c.window, c.msg, got)
				}
				return
			}
			if !ok {
				t.Fatalf("parseExternalLink(%q, %q) refused; want %q", c.window, c.msg, c.want)
			}
			if got != c.want {
				t.Fatalf("parseExternalLink(%q, %q) = %q; want %q", c.window, c.msg, got, c.want)
			}
		})
	}
}

// The injected script must carry the same prefix the Go side strips, and the
// origin it compares against — a mismatch would silently disable the feature.
func TestExternalLinkJSCarriesPrefixAndOrigin(t *testing.T) {
	js := externalLinkJS("https://cloud.example.com")
	for _, want := range []string{`"` + externalLinkPrefix + `"`, `"https://cloud.example.com"`} {
		if !contains(js, want) {
			t.Fatalf("externalLinkJS output missing %s", want)
		}
	}
}

func contains(hay, needle string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
