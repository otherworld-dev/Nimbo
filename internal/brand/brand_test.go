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
