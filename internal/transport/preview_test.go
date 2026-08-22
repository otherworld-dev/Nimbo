package transport

// Preview backs the Android file manager's thumbnails. Nextcloud generates
// previews for images, PDFs and office documents from one endpoint, so a single
// call covers every previewable type — and it must go through this client so
// thumbnails inherit the same auth, timeouts and bandwidth limits as sync.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestPreviewFetchesTheServerPreviewEndpoint(t *testing.T) {
	var gotPath, gotQuery, gotUser string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotUser, _, _ = r.BasicAuth()
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("JPEGBYTES"))
	}))
	defer srv.Close()

	body, err := New(srv.URL, "alice", "pw").Preview(context.Background(), "12345", 256)
	if err != nil {
		t.Fatal(err)
	}

	if string(body) != "JPEGBYTES" {
		t.Fatalf("body = %q, want the image bytes", string(body))
	}
	if gotPath != "/index.php/core/preview" {
		t.Fatalf("path = %q, want /index.php/core/preview", gotPath)
	}
	if gotUser != "alice" {
		t.Fatalf("basic-auth user = %q, want alice (previews must be authenticated)", gotUser)
	}
	q, err := url.ParseQuery(gotQuery)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"fileId": "12345",
		"x":      "256",
		"y":      "256",
		"a":      "1", // preserve aspect ratio rather than cropping to a square
	} {
		if got := q.Get(key); got != want {
			t.Errorf("query %s = %q, want %q (full query %q)", key, got, want, gotQuery)
		}
	}
}

// A missing or un-previewable file must surface as an error, not as zero bytes
// that would render as a broken tile.
func TestPreviewReportsHTTPFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := New(srv.URL, "alice", "pw").Preview(context.Background(), "404", 128); err == nil {
		t.Fatal("want an error for a 404 preview, got nil")
	}
}

func TestPreviewRejectsABlankFileID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a blank file id must not reach the server")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if _, err := New(srv.URL, "alice", "pw").Preview(context.Background(), "", 128); err == nil {
		t.Fatal("want an error for a blank file id, got nil")
	}
}
