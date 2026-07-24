// Package update checks the project's Gitea releases for a newer version. It is
// transport-only: it reports what the latest release is and whether it's newer
// than the running build; applying the update is the installer's job.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/otherworld/nimbo/internal/brand"
)

// apiBase / FeedURL come from the brand config so a white-label build checks
// and installs from the reseller's own release feed. apiBase is the GitHub
// releases API root; FeedURL is the App Installer feed the "Update now" action
// targets.
func apiBaseURL() string { return brand.Current.APIBase }

// FeedURL is the App Installer feed (stable latest/download redirect).
var FeedURL = brand.Current.FeedURL

// Asset is a downloadable file attached to a release.
type Asset struct {
	Name        string `json:"name"`
	DownloadURL string `json:"browser_download_url"`
	Size        int64  `json:"size"`
}

// Release is one published release.
type Release struct {
	Tag        string  `json:"tag_name"`
	Name       string  `json:"name"`
	URL        string  `json:"html_url"`
	Body       string  `json:"body"` // release notes (the in-app update prompt shows these)
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	Assets     []Asset `json:"assets"`
}

// Asset returns the first asset whose name contains sub (case-insensitive), e.g.
// ".msix" or ".appinstaller". Empty Asset if none.
func (r Release) Asset(sub string) Asset {
	sub = strings.ToLower(sub)
	for _, a := range r.Assets {
		if strings.Contains(strings.ToLower(a.Name), sub) {
			return a
		}
	}
	return Asset{}
}

// Latest returns the newest published (non-draft, non-prerelease) release.
func Latest(ctx context.Context) (Release, error) { return latest(ctx, apiBaseURL()) }

func latest(ctx context.Context, base string) (Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/releases?per_page=10", nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return Release{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("releases: %s", resp.Status)
	}
	var rels []Release
	if err := json.NewDecoder(resp.Body).Decode(&rels); err != nil {
		return Release{}, err
	}
	for _, r := range rels {
		if !r.Draft && !r.Prerelease {
			return r, nil
		}
	}
	return Release{}, nil // none published yet
}

// Check returns the latest release and whether it is newer than current. A
// non-release current (e.g. "dev") is treated as up to date so dev builds aren't
// nagged.
func Check(ctx context.Context, current string) (Release, bool, error) {
	rel, err := Latest(ctx)
	if err != nil {
		return Release{}, false, err
	}
	if rel.Tag == "" {
		return rel, false, nil
	}
	return rel, newer(rel.Tag, current), nil
}

// newer reports whether release tag a is a higher version than b. Four
// components are compared (major.minor.build.revision) because Nimbo's user
// version stays 0.1.0 while every build bumps only the 4th (revision) field —
// comparing three would never detect a release.
func newer(a, b string) bool {
	pa, oka := parseSemver(a)
	pb, okb := parseSemver(b)
	if !oka || !okb {
		return false // can't compare (e.g. "dev") → don't claim an update
	}
	for i := 0; i < 4; i++ {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

// parseSemver parses "v1.2.3.4" / "v1.2.3" / "1.2" / "1" into
// [major,minor,build,revision]; absent components are zero.
func parseSemver(s string) ([4]int, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return [4]int{}, false
	}
	if i := strings.IndexAny(s, "-+"); i >= 0 { // strip pre-release/build metadata
		s = s[:i]
	}
	var out [4]int
	for i, part := range strings.SplitN(s, ".", 4) {
		n, err := strconv.Atoi(part)
		if err != nil {
			return [4]int{}, false
		}
		out[i] = n
	}
	return out, true
}
