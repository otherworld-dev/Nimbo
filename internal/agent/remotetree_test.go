package agent

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

// The adopt scan (RemoteTree) is minutes long on a big account and is exactly
// the crawl a cautious user cancels and retries — so it must be checkpoint-
// backed: a repeat scan reuses cached listings and only re-fetches what
// changed, instead of restarting cold.
func TestRemoteTreeResumesFromCheckpoint(t *testing.T) {
	f := newFakeDAV(map[string]davNode{
		"":       {isDir: true, etag: "e-root"},
		"a":      {isDir: true, etag: "e-a"},
		"a/b":    {isDir: true, etag: "e-b"},
		"f1":     {etag: "e-f1", body: "one"},
		"a/f2":   {etag: "e-f2", body: "two"},
		"a/b/f3": {etag: "e-f3", body: "three"},
	})
	srv := httptest.NewServer(f)
	defer srv.Close()
	e, _ := newHookEngine(t, srv.URL)

	// Pass 1: a/b's listing dies mid-crawl (as a cancel or network blip would).
	f.setFailPF("a/b", 403)
	if _, err := e.RemoteTree(context.Background(), "", nil, nil); err == nil {
		t.Fatal("scan with a failing dir must fail")
	}

	// Pass 2: healed. The already-listed dir must come from the checkpoint.
	f.clearFailPF("a/b")
	remote, err := e.RemoteTree(context.Background(), "", nil, nil)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got := f.pfCount("a"); got != 1 {
		t.Fatalf("dir a PROPFINDed %d times across both passes, want 1 (retry must reuse the cached listing)", got)
	}
	if _, ok := remote["a/b/f3"]; !ok {
		t.Fatal("retry missing the previously-failed subtree")
	}
	// The result must be complete and carry what adopt classification needs —
	// CRUCIALLY LastModified, on the REPLAYED entries: the checkpoint codec once
	// dropped it, so a warm re-scan classified every on-server file as a
	// conflict (live incident: 556k conflicted copies / 478 GB upload offered;
	// only the confirm dialog stopped it).
	want := time.Unix(1700000000, 0)
	for _, rel := range []string{"a/f2", "f1", "a/b/f3"} {
		r, ok := remote[rel]
		if !ok || r.ETag == "" {
			t.Fatalf("remote[%s] incomplete: %+v", rel, r)
		}
		if !r.LastModified.Equal(want) {
			t.Errorf("remote[%s].LastModified = %v, want %v (checkpoint replay must preserve mtimes)", rel, r.LastModified, want)
		}
	}
}
