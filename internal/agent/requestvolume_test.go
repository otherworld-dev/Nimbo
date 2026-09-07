package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Deck #599: the access log showed one MKCOL of the pair's remote root per
// sync pass, forever — ensurePair re-created a collection that has existed
// since the first pass. Creating it once per engine run is enough; a scan
// 404s loudly if the root later vanishes server-side.
func TestEnsurePairCreatesTheRemoteRootOnce(t *testing.T) {
	f := newFakeDAV(map[string]davNode{
		"": {isDir: true, etag: "e-root"},
	})
	srv := httptest.NewServer(f)
	defer srv.Close()
	e, _ := newHookEngine(t, srv.URL)

	p := Pair{LocalDir: t.TempDir(), RemoteRoot: "sub"}
	for i := 0; i < 3; i++ {
		if err := e.ensurePair(context.Background(), p); err != nil {
			t.Fatalf("ensurePair pass %d: %v", i, err)
		}
	}
	f.mu.Lock()
	got := f.mkcols["sub"]
	f.mu.Unlock()
	if got != 1 {
		t.Fatalf("remote root MKCOLed %d times across 3 passes, want 1", got)
	}
}

// The 5-minute shares poll was 47%% of Nimbo's idle traffic (868 of the last
// 2000 requests on the reporter's server). The poll stretches to 30 minutes;
// freshness comes from refreshSharesSoon, fired off the server's own
// notify_notification push (a new share raises a notification). It must
// coalesce — notification events arrive in bursts.
func TestRefreshSharesSoonIsThrottled(t *testing.T) {
	var shareCalls int
	f := newFakeDAV(map[string]davNode{"": {isDir: true, etag: "e-root"}})
	mux := http.NewServeMux()
	mux.HandleFunc("/ocs/v2.php/apps/files_sharing/api/v1/shares", func(w http.ResponseWriter, r *http.Request) {
		shareCalls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ocs": map[string]any{
				"meta": map[string]any{"status": "ok", "statuscode": 200},
				"data": []any{},
			},
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/remote.php/") {
			f.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	e, _ := newHookEngine(t, srv.URL)

	ctx := context.Background()
	e.refreshSharesSoon(ctx)
	e.refreshSharesSoon(ctx) // burst: must coalesce into the first refresh
	if shareCalls != 2 {     // ListAllShares = own + received = 2 GETs
		t.Fatalf("share endpoint saw %d calls after a burst, want 2 (one refresh)", shareCalls)
	}

	// Once the throttle window has passed, the next event refreshes again.
	e.sharedMu.Lock()
	e.sharesRefreshedAt = time.Now().Add(-2 * sharesEventThrottle)
	e.sharedMu.Unlock()
	e.refreshSharesSoon(ctx)
	if shareCalls != 4 {
		t.Fatalf("share endpoint saw %d calls after the window passed, want 4", shareCalls)
	}
}

// #599: without push the poll ran every 15s — ~240 PROPFINDs/hour against
// servers that never signed up for it. 30s matches the official client; with
// push connected the poll is only a safety net and stays at 5 minutes (a 15m
// backoff was tried and reverted — too long a worst case for a missed push).
func TestPollIntervalMatchesPushAvailability(t *testing.T) {
	if got := pollIntervalFor(false); got != 30*time.Second {
		t.Errorf("no push: poll = %v, want 30s", got)
	}
	if got := pollIntervalFor(true); got != 5*time.Minute {
		t.Errorf("push: poll = %v, want 5m", got)
	}
}
