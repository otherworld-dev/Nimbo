package transfer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/otherworld/nimbo/internal/engine"
	"github.com/otherworld/nimbo/internal/state"
	"github.com/otherworld/nimbo/internal/transport"
)

// conflictServer serves "remote" for GETs (or getStatus instead) and accepts
// PUTs, counting the GETs.
func conflictServer(t *testing.T, getStatus int) (*transport.Client, *atomic.Int32) {
	t.Helper()
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			gets.Add(1)
			if getStatus != 0 {
				w.WriteHeader(getStatus)
				return
			}
			w.Header().Set("OC-FileId", "fid")
			_, _ = w.Write([]byte("remote"))
		case http.MethodPut:
			w.Header().Set("OC-ETag", `"put"`)
			w.Header().Set("OC-FileId", "fid-put")
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return transport.New(srv.URL, "u", "p"), &gets
}

func conflictExecutor(t *testing.T, c *transport.Client) *Executor {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "doc.txt"), []byte("local edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"), "acct", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Executor{Client: c, State: st, PairKey: "P", LocalRoot: dir,
		Remote: map[string]engine.RemoteState{"doc.txt": {Path: "doc.txt", ETag: "e2", FileID: "fid"}}}
}

// Keeping both sets the local file aside and puts the server's version in its
// place. It set the local file aside FIRST, so when the download then failed
// (a damaged server copy, a dropped connection) the user's file had been moved
// away from its name and nothing was in its place.
func TestKeepBothLeavesTheLocalFileAloneWhenTheDownloadFails(t *testing.T) {
	c, _ := conflictServer(t, http.StatusForbidden)
	ex := conflictExecutor(t, c)
	if err := ex.keepBoth(context.Background(), "doc.txt"); err == nil {
		t.Fatal("keepBoth reported success although the download failed")
	}
	if b, err := os.ReadFile(filepath.Join(ex.LocalRoot, "doc.txt")); err != nil || string(b) != "local edit" {
		t.Fatalf("the local file was moved or changed: %q err=%v", b, err)
	}
	entries, _ := os.ReadDir(ex.LocalRoot)
	for _, en := range entries {
		if strings.Contains(en.Name(), "conflicted copy") {
			t.Fatalf("a conflicted copy was made for a download that failed: %s", en.Name())
		}
	}
}

// Resolving a divergent conflict fetched the server's copy to compare it,
// threw it away, then fetched it again to keep both: twice the bandwidth,
// which for a 24 GB file is minutes of it.
func TestResolveConflictFetchesTheServerCopyOnce(t *testing.T) {
	c, gets := conflictServer(t, 0)
	ex := conflictExecutor(t, c)
	if _, err := ex.resolveConflict(context.Background(), engine.Action{Kind: engine.ActConflict, Path: "doc.txt"}); err != nil {
		t.Fatalf("resolveConflict: %v", err)
	}
	if n := gets.Load(); n != 1 {
		t.Fatalf("the server copy was fetched %d times, want 1", n)
	}
	if b, _ := os.ReadFile(filepath.Join(ex.LocalRoot, "doc.txt")); string(b) != "remote" {
		t.Fatalf("the server's version did not take the original name: %q", b)
	}
}
