package transport

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSearchByNameRequest pins the SEARCH we send. It matters because the
// endpoint, the scope and the escaping are each a silent failure if wrong: a
// bad scope returns everything, a bad literal returns nothing, and neither
// looks like an error.
func TestSearchByNameRequest(t *testing.T) {
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
	if _, err := c.SearchByName(context.Background(), "report", 25); err != nil {
		t.Fatalf("SearchByName: %v", err)
	}
	if gotMethod != "SEARCH" {
		t.Errorf("method = %q, want SEARCH", gotMethod)
	}
	// The search endpoint is the DAV root, not the user's files collection.
	if gotPath != "/remote.php/dav/" {
		t.Errorf("path = %q, want /remote.php/dav/", gotPath)
	}
	for _, want := range []string{
		"<d:scope><d:href>/files/alice</d:href>",
		"%report%",
		"<d:nresults>25</d:nresults>",
		"getcontentlength", // the same properties a listing gets
	} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("body missing %q\n---\n%s", want, gotBody)
		}
	}
}

// TestSearchByNameEscapesTheTerm guards the two ways a term breaks the request:
// XML metacharacters corrupt the document, and SQL-LIKE wildcards silently
// widen the search to everything.
func TestSearchByNameEscapesTheTerm(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusMultiStatus)
		io.WriteString(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:"/>`)
	}))
	defer srv.Close()

	c := New(srv.URL, "alice", "pw")
	if _, err := c.SearchByName(context.Background(), `a<b&c"%_`, 10); err != nil {
		t.Fatalf("SearchByName: %v", err)
	}
	if strings.Contains(gotBody, "a<b&c") {
		t.Errorf("term was not XML-escaped:\n%s", gotBody)
	}
	if !strings.Contains(gotBody, "&lt;") || !strings.Contains(gotBody, "&amp;") {
		t.Errorf("expected escaped entities in:\n%s", gotBody)
	}
	// A literal % typed by the user must not act as "match anything".
	if !strings.Contains(gotBody, `\%`) || !strings.Contains(gotBody, `\_`) {
		t.Errorf("LIKE wildcards were not escaped:\n%s", gotBody)
	}
}

// TestSearchByNameParsesHits is the point of doing this over WebDAV at all:
// hits come back with real, account-relative paths, so they can be opened in
// the app rather than only in a browser.
func TestSearchByNameParsesHits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMultiStatus)
		io.WriteString(w, `<?xml version="1.0"?>
<d:multistatus xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns" xmlns:nc="http://nextcloud.org/ns">
  <d:response>
    <d:href>/remote.php/dav/files/alice/Documents/report.pdf</d:href>
    <d:propstat><d:status>HTTP/1.1 200 OK</d:status><d:prop>
      <d:getetag>&quot;e1&quot;</d:getetag>
      <d:getcontentlength>512</d:getcontentlength>
      <d:getcontenttype>application/pdf</d:getcontenttype>
      <oc:fileid>99</oc:fileid>
    </d:prop></d:propstat>
  </d:response>
  <d:response>
    <d:href>/remote.php/dav/files/alice/Reports/</d:href>
    <d:propstat><d:status>HTTP/1.1 200 OK</d:status><d:prop>
      <d:resourcetype><d:collection/></d:resourcetype>
      <d:getetag>&quot;e2&quot;</d:getetag>
    </d:prop></d:propstat>
  </d:response>
</d:multistatus>`)
	}))
	defer srv.Close()

	c := New(srv.URL, "alice", "pw")
	hits, err := c.SearchByName(context.Background(), "report", 25)
	if err != nil {
		t.Fatalf("SearchByName: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2: %+v", len(hits), hits)
	}
	if hits[0].Path != "Documents/report.pdf" || hits[0].Size != 512 {
		t.Errorf("first hit = %+v, want the account-relative path and its size", hits[0])
	}
	if hits[1].Path != "Reports" || !hits[1].IsDir {
		t.Errorf("second hit = %+v, want a directory at Reports", hits[1])
	}
}

// TestSearchByNameRejectsABlankTerm: a blank LIKE matches the whole account,
// which would look like a working search returning every file the user owns.
func TestSearchByNameRejectsABlankTerm(t *testing.T) {
	c := New("https://cloud.example.com", "alice", "pw")
	for _, term := range []string{"", "   "} {
		hits, err := c.SearchByName(context.Background(), term, 25)
		if err == nil {
			t.Errorf("SearchByName(%q) = %d hits, nil; want an error", term, len(hits))
		}
	}
}
