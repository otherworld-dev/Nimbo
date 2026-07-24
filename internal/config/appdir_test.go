package config

import "testing"

func TestBrandSlug(t *testing.T) {
	cases := map[string]string{
		"Nimbo":          "nimbo",
		"AcmeCloud":      "acmecloud",
		"AcmeCloud Sync": "acmecloud-sync",
		"Blue.Sync_2":    "blue-sync-2",
		"-Trim-Me-":      "trim-me",
		"  ":             "nimbo", // empty after trim → fallback
		"":               "nimbo",
		"!!!":            "nimbo", // no usable chars → fallback
	}
	for in, want := range cases {
		if got := brandSlug(in); got != want {
			t.Errorf("brandSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

// The embedded stock brand.json has appId "Nimbo", which must slug to "nimbo"
// so existing stock installs keep their config/data directory (no migration).
func TestStockAppDirUnchanged(t *testing.T) {
	if appDir != "nimbo" {
		t.Fatalf("stock appDir = %q, want \"nimbo\"", appDir)
	}
}
