package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v1.2.0", "v1.1.9", true},
		{"v1.2.0", "v1.2.0", false},
		{"v1.2.0", "v1.3.0", false},
		{"v2.0.0", "v1.9.9", true},
		{"v1.0.1", "v1.0.0", true},
		{"1.2.3", "v1.2.2", true},
		{"v1.2.0", "dev", false}, // unparseable current → no update claimed
		{"dev", "v1.0.0", false},
		{"v1.2.0-rc1", "v1.2.0", false}, // pre-release metadata stripped → equal
	}
	for _, c := range cases {
		if got := newer(c.a, c.b); got != c.want {
			t.Errorf("newer(%q,%q)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}

// releasesFixture serves a draft, a pre-release and a stable release, newest
// first — the shape the GitHub releases API returns.
func releasesFixture(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[
			{"tag_name":"v2.0.0","draft":true,"assets":[]},
			{"tag_name":"v1.9.0","prerelease":true,"assets":[]},
			{"tag_name":"v1.5.0","name":"1.5","html_url":"http://x/1.5",
			 "assets":[{"name":"Nimbo-1.5.0.msix","browser_download_url":"http://x/n.msix","size":42}]}
		]`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// unorderedFixture models what GitHub actually served on 2026-07-25: the
// /releases list is ordered by created_at, created_at is the TAGGED COMMIT's
// date, and in this repo several releases share one — so the order is NOT
// version order. Live, the .169 pre-release came back THIRD, below .168 and
// .167, which made the beta channel report "you're up to date (v0.1.0.168)"
// while .169 existed.
//
// The newest stable is pushed down here too, which the live response didn't
// happen to do. Under a created_at tie it just as easily could, and the same
// take-the-first-match defect would then hand the STABLE channel an older
// release — so both channels are covered.
func unorderedFixture(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[
			{"tag_name":"v0.1.0.167","prerelease":false,"assets":[]},
			{"tag_name":"v0.1.0.169","prerelease":true,"assets":[]},
			{"tag_name":"v0.1.0.168","prerelease":false,"assets":[]},
			{"tag_name":"v0.1.0.166","prerelease":false,"assets":[]}
		]`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// The list order must not decide the answer — the highest VERSION must, on both
// channels. Regression for the beta channel silently reporting "up to date".
func TestLatestIgnoresListOrder(t *testing.T) {
	cases := []struct {
		name string
		beta bool
		want string
	}{
		{"beta channel takes the newest overall", true, "v0.1.0.169"},
		{"stable channel takes the newest stable", false, "v0.1.0.168"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rel, err := latest(context.Background(), unorderedFixture(t), c.beta)
			if err != nil {
				t.Fatal(err)
			}
			if rel.Tag != c.want {
				t.Fatalf("latest(beta=%v) = %q; want %q", c.beta, rel.Tag, c.want)
			}
		})
	}
}

// The same defect seen through Check: an out-of-order list must still mark an
// available update rather than claiming the running build is current.
func TestCheckSeesNewerDespiteListOrder(t *testing.T) {
	rel, avail, err := checkAt(context.Background(), unorderedFixture(t), "v0.1.0.168", true)
	if err != nil {
		t.Fatal(err)
	}
	if !avail || rel.Tag != "v0.1.0.169" {
		t.Fatalf("Check = (%q, avail=%v); want (v0.1.0.169, avail=true)", rel.Tag, avail)
	}
}

func TestLatestChannels(t *testing.T) {
	cases := []struct {
		name string
		beta bool
		want string
	}{
		{"stable channel skips the pre-release", false, "v1.5.0"},
		{"beta channel takes the pre-release", true, "v1.9.0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rel, err := latest(context.Background(), releasesFixture(t), c.beta)
			if err != nil {
				t.Fatal(err)
			}
			if rel.Tag != c.want {
				t.Fatalf("latest(beta=%v) = %q; want %q", c.beta, rel.Tag, c.want)
			}
		})
	}
}

// A draft must never be offered on either channel: its assets need auth.
func TestLatestNeverReturnsDraft(t *testing.T) {
	for _, beta := range []bool{false, true} {
		rel, err := latest(context.Background(), releasesFixture(t), beta)
		if err != nil {
			t.Fatal(err)
		}
		if rel.Tag == "v2.0.0" {
			t.Fatalf("latest(beta=%v) returned the draft", beta)
		}
	}
}

func TestLatestFindsAsset(t *testing.T) {
	rel, err := latest(context.Background(), releasesFixture(t), false)
	if err != nil {
		t.Fatal(err)
	}
	if a := rel.Asset(".msix"); a.DownloadURL != "http://x/n.msix" {
		t.Fatalf("asset lookup failed: %+v", a)
	}
}

// Check's whole job is `latest` + `newer`: fetch the release the channel
// allows, then report whether its tag is newer than the running version. Check
// itself can't be exercised directly here because it hard-codes the real API
// base (via Latest); this drives the same two calls Check makes, in the same
// order, against a pre-release tag on the beta channel — the scenario this
// branch adds and the one finding 1's per_page regression would have broken.
func TestCheckComposesAvailOnBetaPrerelease(t *testing.T) {
	base := releasesFixture(t)
	rel, err := latest(context.Background(), base, true)
	if err != nil {
		t.Fatal(err)
	}
	if rel.Tag != "v1.9.0" {
		t.Fatalf("latest(beta=true) = %q; want v1.9.0 (the pre-release)", rel.Tag)
	}
	cases := []struct {
		current string
		want    bool
	}{
		{"v1.0.0", true},  // older stable -> the pre-release is available
		{"v1.9.0", false}, // already running it -> not available
		{"v2.0.0", false}, // ahead of it (running the skipped draft's version) -> not available
	}
	for _, c := range cases {
		if got := newer(rel.Tag, c.current); got != c.want {
			t.Errorf("avail = newer(%q, %q) = %v, want %v", rel.Tag, c.current, got, c.want)
		}
	}
}

func TestNewerIsExported(t *testing.T) {
	if !Newer("v0.1.0.168", "v0.1.0.167") {
		t.Fatal("Newer should report 168 newer than 167")
	}
	if Newer("v0.1.0.167", "v0.1.0.168") {
		t.Fatal("Newer should not report 167 newer than 168")
	}
}
