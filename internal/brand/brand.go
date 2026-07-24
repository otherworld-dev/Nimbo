// Package brand centralises every white-labellable identity value — product
// name, company, URLs, update feed, accent colour, AUMID app id — in one place,
// loaded from an embedded brand.json at build time. A reseller produces a
// branded build by editing brand.json (the app icon regenerates from the accent
// colour) and setting the MSIX manifest Name/Publisher — no code changes. See
// packaging/whitelabel/.
package brand

import (
	_ "embed"
	"encoding/json"
	"log/slog"
)

//go:embed brand.json
var brandJSON []byte

// Brand is the set of identity values that differ between the stock Nimbo build
// and a white-label build.
type Brand struct {
	Name         string `json:"name"`         // product name shown to users ("Nimbo")
	Company      string `json:"company"`      // legal entity ("Otherworld Dev Ltd")
	Tagline      string `json:"tagline"`      // short descriptor
	Website      string `json:"website"`      // marketing site
	SupportEmail string `json:"supportEmail"` // shown in About / business page
	FeedURL      string `json:"feedUrl"`      // App Installer feed (in-app update target check)
	APIBase      string `json:"apiBase"`      // GitHub releases API root for the update check
	AccentHex    string `json:"accentHex"`    // brand accent (tray badge, UI accent fallback)
	AppID        string `json:"appId"`        // MSIX Application Id (AUMID) + self-update task name + the config/data dir name (slugged)
}

// Current is the active brand for this build.
var Current Brand

func init() {
	if err := json.Unmarshal(brandJSON, &Current); err != nil {
		// A malformed brand.json is a build mistake; fall back to safe stock
		// values so the app still runs while it's noticed.
		slog.Error("brand.json invalid, using stock defaults", "err", err)
		Current = Brand{
			Name: "Nimbo", Company: "Otherworld Dev Ltd",
			Website: "https://www.nimbosync.com", SupportEmail: "contact@otherworld.dev",
			FeedURL:   "https://github.com/otherworld-dev/Nimbo/releases/latest/download/Nimbo.appinstaller",
			APIBase:   "https://api.github.com/repos/otherworld-dev/Nimbo",
			AccentHex: "#5856E0", AppID: "Nimbo",
		}
	}
}
