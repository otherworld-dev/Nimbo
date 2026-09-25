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

// Nextcloud's navigation API hands back hrefs and icons that already carry the
// server's webroot ("/nextcloud/apps/..."), while our own calls pass paths
// relative to the webroot ("/index.php/..."). Both must land on the same
// server, once — GitHub #16 was every dock icon at /nextcloud/nextcloud/.
func TestResolveServerHref(t *testing.T) {
	cases := []struct {
		name   string
		server string
		href   string
		want   string
	}{
		{"root install, nav href", "https://cloud.example.com", "/apps/files/", "https://cloud.example.com/apps/files/"},
		{"root install, trailing slash", "https://cloud.example.com/", "/index.php/apps/theming/icon/deck", "https://cloud.example.com/index.php/apps/theming/icon/deck"},
		{"subpath, nav href carries webroot", "https://example.com/nextcloud", "/nextcloud/index.php/apps/files/", "https://example.com/nextcloud/index.php/apps/files/"},
		{"subpath, nav icon carries webroot", "https://example.com/nextcloud/", "/nextcloud/apps/files/img/app.svg", "https://example.com/nextcloud/apps/files/img/app.svg"},
		{"subpath, webroot-relative path", "https://example.com/nextcloud", "/index.php/apps/theming/icon/deck", "https://example.com/nextcloud/index.php/apps/theming/icon/deck"},
		{"subpath, no leading slash", "https://example.com/nextcloud", "apps/files/", "https://example.com/nextcloud/apps/files/"},
		{"subpath, webroot is only a name prefix", "https://example.com/nextcloud", "/nextcloudish/x", "https://example.com/nextcloud/nextcloudish/x"},
		{"subpath, bare webroot", "https://example.com/nextcloud", "/nextcloud", "https://example.com/nextcloud"},
		{"absolute URL untouched", "https://example.com/nextcloud", "https://other.example.org/a", "https://other.example.org/a"},
		{"mailto untouched", "https://example.com/nextcloud", "mailto:help@example.com", "mailto:help@example.com"},
		{"empty", "https://example.com/nextcloud", "", ""},
	}
	for _, c := range cases {
		if got := resolveServerHref(c.server, c.href); got != c.want {
			t.Errorf("%s: resolveServerHref(%q, %q) = %q, want %q", c.name, c.server, c.href, got, c.want)
		}
	}
}
