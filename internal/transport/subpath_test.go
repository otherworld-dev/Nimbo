package transport

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A Nextcloud installed at a URL subpath ("https://host/nextcloud") answers
// PROPFIND with hrefs that carry that subpath:
// "/nextcloud/remote.php/dav/files/alice/My PDFs/". The client must still hand
// back "My PDFs" — GitHub #3/#4 (v0.1.0.269): the prefix strip expected the
// href to START with /remote.php/dav/files/<user>, silently did nothing when
// it didn't, and every entry came back as
// "nextcloud/remote.php/dav/files/alice/My PDFs". Re-prepended on the next
// request, that 404s, so sync, the adopt scan and every download failed
// against such a server.
func TestPropFindStripsSubpathInstallPrefix(t *testing.T) {
	const body = `<?xml version="1.0"?>
<d:multistatus xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns">
  <d:response>
    <d:href>/nextcloud/remote.php/dav/files/alice/</d:href>
    <d:propstat><d:status>HTTP/1.1 200 OK</d:status><d:prop>
      <d:getetag>&quot;e-root&quot;</d:getetag>
      <d:resourcetype><d:collection/></d:resourcetype>
    </d:prop></d:propstat>
  </d:response>
  <d:response>
    <d:href>/nextcloud/remote.php/dav/files/alice/My%20PDFs/</d:href>
    <d:propstat><d:status>HTTP/1.1 200 OK</d:status><d:prop>
      <d:getetag>&quot;e-pdfs&quot;</d:getetag>
      <d:resourcetype><d:collection/></d:resourcetype>
    </d:prop></d:propstat>
  </d:response>
  <d:response>
    <d:href>/nextcloud/remote.php/dav/files/alice/notes.txt</d:href>
    <d:propstat><d:status>HTTP/1.1 200 OK</d:status><d:prop>
      <d:getetag>&quot;e-notes&quot;</d:getetag>
      <d:resourcetype/>
    </d:prop></d:propstat>
  </d:response>
</d:multistatus>`
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusMultiStatus)
		io.WriteString(w, body)
	}))
	defer srv.Close()

	c := New(srv.URL+"/nextcloud", "alice", "pw")
	entries, err := c.PropFind(context.Background(), "", 1)
	if err != nil {
		t.Fatalf("PropFind: %v", err)
	}
	// The request itself already went to the right place — the bug was only
	// ever in reading the answer.
	if want := "/nextcloud/remote.php/dav/files/alice/"; gotPath != want {
		t.Errorf("request path = %q, want %q", gotPath, want)
	}
	byPath := map[string]Entry{}
	for _, e := range entries {
		if strings.Contains(e.Path, "remote.php") {
			t.Errorf("entry path %q still carries the DAV prefix", e.Path)
		}
		byPath[e.Path] = e
	}
	if _, ok := byPath[""]; !ok {
		t.Errorf("the listed directory itself must come back as %q; got paths %v", "", keys(byPath))
	}
	if e, ok := byPath["My PDFs"]; !ok || !e.IsDir {
		t.Errorf("want a directory entry %q; got paths %v", "My PDFs", keys(byPath))
	}
	if e, ok := byPath["notes.txt"]; !ok || e.IsDir {
		t.Errorf("want a file entry %q; got paths %v", "notes.txt", keys(byPath))
	}
}

// An href the client can't place under its own files root is a hard error,
// never a path. Passing it through untrimmed is how #3/#4 turned a
// misconfiguration into a sync that planned deletes; skipping it silently
// would be worse still — an empty listing reads as "everything was deleted on
// the server".
func TestPropFindRejectsHrefOutsideFilesRoot(t *testing.T) {
	const body = `<?xml version="1.0"?>
<d:multistatus xmlns:d="DAV:">
  <d:response>
    <d:href>/somewhere/else/entirely/x.txt</d:href>
    <d:propstat><d:status>HTTP/1.1 200 OK</d:status><d:prop>
      <d:getetag>&quot;e&quot;</d:getetag>
      <d:resourcetype/>
    </d:prop></d:propstat>
  </d:response>
</d:multistatus>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMultiStatus)
		io.WriteString(w, body)
	}))
	defer srv.Close()

	c := New(srv.URL, "alice", "pw")
	entries, err := c.PropFind(context.Background(), "", 1)
	if err == nil {
		t.Fatalf("PropFind accepted a foreign href; entries = %+v", entries)
	}
}

func keys(m map[string]Entry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
