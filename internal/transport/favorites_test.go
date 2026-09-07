package transport

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestParseResponseFavorite covers oc:favorite on an ordinary listing. Without
// it a star toggle in a file browser has no idea which files are already
// starred, so it can only ever guess at its own state.
//
// The fixture mirrors what Nextcloud actually returns: "1" for a favourite,
// "0" for a plain file, and — for a server without the property — a 404
// propstat carrying no value at all.
func TestParseResponseFavorite(t *testing.T) {
	const body = `<?xml version="1.0"?>
<d:multistatus xmlns:d="DAV:" xmlns:nc="http://nextcloud.org/ns" xmlns:oc="http://owncloud.org/ns">
  <d:response>
    <d:href>/remote.php/dav/files/alice/starred.txt</d:href>
    <d:propstat><d:status>HTTP/1.1 200 OK</d:status><d:prop>
      <d:getetag>&quot;e1&quot;</d:getetag>
      <oc:favorite>1</oc:favorite>
    </d:prop></d:propstat>
  </d:response>
  <d:response>
    <d:href>/remote.php/dav/files/alice/plain.txt</d:href>
    <d:propstat><d:status>HTTP/1.1 200 OK</d:status><d:prop>
      <d:getetag>&quot;e2&quot;</d:getetag>
      <oc:favorite>0</oc:favorite>
    </d:prop></d:propstat>
  </d:response>
  <d:response>
    <d:href>/remote.php/dav/files/alice/silent.txt</d:href>
    <d:propstat><d:status>HTTP/1.1 200 OK</d:status><d:prop>
      <d:getetag>&quot;e3&quot;</d:getetag>
    </d:prop></d:propstat>
    <d:propstat><d:status>HTTP/1.1 404 Not Found</d:status><d:prop>
      <oc:favorite/>
    </d:prop></d:propstat>
  </d:response>
</d:multistatus>`
	var ms multistatus
	if err := xml.Unmarshal([]byte(body), &ms); err != nil {
		t.Fatal(err)
	}
	c := New("https://cloud.example.com", "alice", "pw")
	byPath := map[string]Entry{}
	for _, r := range ms.Responses {
		e, ok, err := c.parseResponse(r)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			byPath[e.Path] = e
		}
	}
	if e := byPath["starred.txt"]; !e.IsFavorite {
		t.Errorf("starred.txt: IsFavorite = false, want true")
	}
	if e := byPath["plain.txt"]; e.IsFavorite {
		t.Errorf("plain.txt: IsFavorite = true, want false")
	}
	// A server that doesn't report the property is not a server where every
	// file is favourited.
	if e := byPath["silent.txt"]; e.IsFavorite {
		t.Errorf("silent.txt: IsFavorite = true, want false when unreported")
	}
}

// TestSetFavorite checks the PROPPATCH we send: the right method, at the right
// URL, setting oc:favorite to 1 or removing it via 0.
func TestSetFavorite(t *testing.T) {
	for _, tc := range []struct {
		name string
		fav  bool
		want string
	}{
		{"star", true, "<oc:favorite>1</oc:favorite>"},
		{"unstar", false, "<oc:favorite>0</oc:favorite>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotMethod, gotPath, gotBody string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath = r.Method, r.URL.Path
				b, _ := io.ReadAll(r.Body)
				gotBody = string(b)
				w.WriteHeader(http.StatusMultiStatus)
				io.WriteString(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:"/>`)
			}))
			defer srv.Close()

			c := New(srv.URL, "alice", "pw")
			if err := c.SetFavorite(context.Background(), "Docs/report.pdf", tc.fav); err != nil {
				t.Fatalf("SetFavorite: %v", err)
			}
			if gotMethod != "PROPPATCH" {
				t.Errorf("method = %q, want PROPPATCH", gotMethod)
			}
			if want := "/remote.php/dav/files/alice/Docs/report.pdf"; gotPath != want {
				t.Errorf("path = %q, want %q", gotPath, want)
			}
			if !strings.Contains(gotBody, tc.want) {
				t.Errorf("body = %q, want it to contain %q", gotBody, tc.want)
			}
		})
	}
}

// TestSetFavoriteRejectsRoot guards against starring the account root, which
// the server answers with a confusing error rather than a refusal.
func TestSetFavoriteRejectsRoot(t *testing.T) {
	c := New("https://cloud.example.com", "alice", "pw")
	for _, p := range []string{"", "/", "   "} {
		if err := c.SetFavorite(context.Background(), p, true); err == nil {
			t.Errorf("SetFavorite(%q) = nil, want an error", p)
		}
	}
}

// TestFavoritesReportAsksForEveryEntryProperty guards a trap that is invisible
// until it reaches a screen: Favorites returns the same Entry type as a
// directory listing, so a client renders it with the same row code — and a
// REPORT that asks for fewer properties silently yields files of size 0, with
// no content type and no modified date.
func TestFavoritesReportAsksForEveryEntryProperty(t *testing.T) {
	for _, prop := range []string{
		"getcontentlength", // or every file reads as 0 bytes
		"getlastmodified",
		"getcontenttype", // or nothing is previewable and icons go generic
		"resourcetype",
		"getetag",
		"fileid",
		"size", // oc:size — how a directory reports its own
		"favorite",
	} {
		if !strings.Contains(favoritesReport, prop) {
			t.Errorf("favoritesReport does not request %q; favourite rows will render incomplete", prop)
		}
	}
}
