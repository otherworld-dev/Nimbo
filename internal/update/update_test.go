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

func TestLatestSkipsDraftAndPrerelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[
			{"tag_name":"v2.0.0","draft":true,"assets":[]},
			{"tag_name":"v1.9.0","prerelease":true,"assets":[]},
			{"tag_name":"v1.5.0","name":"1.5","html_url":"http://x/1.5",
			 "assets":[{"name":"Nimbo-1.5.0.msix","browser_download_url":"http://x/n.msix","size":42}]}
		]`))
	}))
	defer srv.Close()

	rel, err := latest(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if rel.Tag != "v1.5.0" {
		t.Fatalf("got tag %q, want v1.5.0 (drafts/prereleases skipped)", rel.Tag)
	}
	if a := rel.Asset(".msix"); a.DownloadURL != "http://x/n.msix" {
		t.Fatalf("asset lookup failed: %+v", a)
	}
}
