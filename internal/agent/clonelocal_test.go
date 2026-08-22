package agent

// Regression coverage for the first-sync bug reported from Nimbo-Android: a
// brand-new pair's first pass runs a download-only bulk clone, which enumerates
// only the REMOTE tree. Pairing a populated local folder with an empty remote
// folder therefore produced zero actions, wrote zero baseline rows, marked the
// clone "done" and reported "Up to date" — with nothing uploaded. On the device
// this looked like "added /storage/emulated/0/DCIM -> empty Testing folder and
// the whole camera roll was ignored"; only reopening the app (whose startup
// reconcile then took the diff path) recovered it.
//
// The classification itself was never wrong — engine.classifyLocalOnly maps a
// local-only path with no baseline to ActUpload. The bug was purely that pass 1
// never reached the diff.

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The first sync of a pair whose local folder already holds files, into an empty
// remote folder, must upload them rather than reporting success having done
// nothing.
func TestFirstSyncUploadsPreExistingLocalFiles(t *testing.T) {
	f := newFakeDAV(map[string]davNode{
		"": {isDir: true, etag: "e-root"}, // empty remote — nothing to download
	})
	srv := httptest.NewServer(f)
	defer srv.Close()
	e, st := newHookEngine(t, srv.URL)

	local := t.TempDir()
	writeLocal(t, local, "photo1.jpg", "one")
	writeLocal(t, local, filepath.Join("sub", "photo2.jpg"), "two")

	if _, err := e.SyncOnce(context.Background(), Pair{LocalDir: local}); err != nil {
		t.Fatal(err)
	}

	want := []string{"photo1.jpg", "sub/photo2.jpg"}
	if got := f.putPaths(); !reflect.DeepEqual(got, want) {
		t.Fatalf("uploaded %v, want %v", got, want)
	}
	if got := f.putBody("photo1.jpg"); got != "one" {
		t.Fatalf("photo1.jpg uploaded as %q, want %q", got, "one")
	}
	if status, _ := st.CloneStatus(PairKey(local, "")); status != "done" {
		t.Fatalf("clone status = %q, want done", status)
	}
}

// A first sync into an empty remote must still leave the pair fully reconciled:
// every uploaded file carries a baseline row, so the NEXT pass is a no-op rather
// than a re-upload.
func TestFirstSyncRecordsBaselineForUploadedFiles(t *testing.T) {
	f := newFakeDAV(map[string]davNode{
		"": {isDir: true, etag: "e-root"},
	})
	srv := httptest.NewServer(f)
	defer srv.Close()
	e, _ := newHookEngine(t, srv.URL)

	local := t.TempDir()
	writeLocal(t, local, "photo1.jpg", "one")

	if _, err := e.SyncOnce(context.Background(), Pair{LocalDir: local}); err != nil {
		t.Fatal(err)
	}
	if got := f.putPaths(); len(got) != 1 {
		t.Fatalf("first pass uploaded %v, want exactly one file", got)
	}

	if _, err := e.SyncOnce(context.Background(), Pair{LocalDir: local}); err != nil {
		t.Fatal(err)
	}
	if got := f.putPaths(); len(got) != 1 {
		t.Fatalf("second pass re-uploaded (paths now %v) — baseline was not recorded", got)
	}
}

func writeLocal(t *testing.T, root, rel, body string) {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
