package agent

// DownloadTo backs "open this file" in the Android file manager. DownloadRange
// already exists but answers with a []byte, which is the wrong shape for a video
// — this streams to disk instead.

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestDownloadToWritesTheRemoteFile(t *testing.T) {
	f := newFakeDAV(map[string]davNode{
		"":        {isDir: true, etag: "e-root"},
		"doc.txt": {etag: "e-doc", body: "hello world"},
	})
	srv := httptest.NewServer(f)
	defer srv.Close()
	e, _ := newHookEngine(t, srv.URL)

	// Into a directory that does not exist yet: the cache path for a download is
	// keyed by file id, so the parent is routinely missing.
	dst := filepath.Join(t.TempDir(), "cache", "12345", "doc.txt")
	if err := e.DownloadTo(context.Background(), "doc.txt", dst); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello world" {
		t.Fatalf("downloaded %q, want %q", string(got), "hello world")
	}
}

// A failed download must not leave a half-written file behind: the cache is
// keyed by file id, so a truncated leftover would be served forever as if it
// were the real thing.
func TestDownloadToLeavesNothingBehindWhenTheFetchFails(t *testing.T) {
	f := newFakeDAV(map[string]davNode{
		"": {isDir: true, etag: "e-root"},
	})
	srv := httptest.NewServer(f)
	defer srv.Close()
	e, _ := newHookEngine(t, srv.URL)

	dst := filepath.Join(t.TempDir(), "missing.txt")
	if err := e.DownloadTo(context.Background(), "missing.txt", dst); err == nil {
		t.Fatal("want an error downloading a file the server does not have")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("a failed download left %s behind (stat err = %v)", dst, err)
	}
}
