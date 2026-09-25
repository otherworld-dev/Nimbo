package brand

import "testing"

// TestStockBrandLoaded checks the embedded brand.json parses into sane stock
// values (a white-label build swaps the file; this guards the default).
func TestStockBrandLoaded(t *testing.T) {
	if Current.Name == "" {
		t.Fatal("brand name is empty — brand.json failed to load")
	}
	for field, v := range map[string]string{
		"name": Current.Name, "company": Current.Company, "website": Current.Website,
		"feedUrl": Current.FeedURL, "apiBase": Current.APIBase, "appId": Current.AppID,
		"helpUrl":   Current.HelpURL,
		"accentHex": Current.AccentHex,
	} {
		if v == "" {
			t.Errorf("brand field %q is empty", field)
		}
	}
	if Current.AccentHex[0] != '#' {
		t.Errorf("accentHex %q is not a hex colour", Current.AccentHex)
	}
}

// TestHelpPage pins how help links are built, including the white-label case
// where a brand has no help site and every help link must disappear.
func TestHelpPage(t *testing.T) {
	cases := []struct {
		base, slug, want string
	}{
		{"https://example.com/help/", "", "https://example.com/help/"},
		{"https://example.com/help/", "file-modes", "https://example.com/help/file-modes.html"},
		{"https://example.com/help", "faq", "https://example.com/help/faq.html"},
		{"https://example.com/help", "", "https://example.com/help/"},
		{"", "faq", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := (Brand{HelpURL: c.base}).HelpPage(c.slug); got != c.want {
			t.Errorf("HelpPage(%q) with base %q = %q, want %q", c.slug, c.base, got, c.want)
		}
	}
}
