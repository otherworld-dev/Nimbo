package transport

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRootID(t *testing.T) {
	var gotDepth, gotBody, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotDepth = r.URL.Path, r.Header.Get("Depth")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusMultiStatus)
		io.WriteString(w, `<?xml version="1.0"?>
<d:multistatus xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns">
 <d:response><d:href>/remote.php/dav/files/alice/</d:href>
  <d:propstat><d:prop><oc:id>00000042ocabc123</oc:id></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat>
 </d:response>
</d:multistatus>`)
	}))
	defer srv.Close()

	c := New(srv.URL, "alice", "pw")
	id, err := c.RootID(context.Background())
	if err != nil || id != "00000042ocabc123" {
		t.Fatalf("RootID = %q, %v", id, err)
	}
	if gotPath != "/remote.php/dav/files/alice/" || gotDepth != "0" || !strings.Contains(gotBody, "<oc:id/>") {
		t.Errorf("request: path=%q depth=%q body=%q", gotPath, gotDepth, gotBody)
	}
}

func TestRootIDErrors(t *testing.T) {
	status, body := http.StatusMultiStatus, `<d:multistatus xmlns:d="DAV:"><d:response><d:href>/x/</d:href><d:propstat><d:prop/><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	defer srv.Close()
	c := New(srv.URL, "alice", "pw")
	if _, err := c.RootID(context.Background()); err == nil || !strings.Contains(err.Error(), "no oc:id") {
		t.Errorf("missing id: err = %v", err)
	}
	status = http.StatusUnauthorized
	if _, err := c.RootID(context.Background()); StatusCode(err) != 401 {
		t.Errorf("401: err = %v", err)
	}
}
