package main

import (
	"sync"
	"time"

	"github.com/otherworld/nimbo/internal/engine"
)

// vfsDisplayPath is the name the activity feed and toasts show for a path an
// on-demand mount reported. The write-back watcher reports LOCAL names, but
// hydration reports the placeholder identity, which is the RAW server name:
// for a disguised file that is ".htaccess.nimboesc" against the watcher's
// ".htaccess". The recorder keys unresolved errors by path, so a failed
// hydration recorded under the raw name could never be cleared by a later
// upload recorded under the local one, and the Status window showed it
// forever (Deck #554). Decode is a no-op on anything that is not an escaped
// name, a nil escaper included, so every report can go through it.
func vfsDisplayPath(esc *engine.Escaper, remote string) string {
	shown, _ := esc.Decode(remote)
	return shown
}

// downloadDedupe drops the second report of one download failure. A pinned
// file that fails to download is reported twice: first by the provider's
// stream, with the real error (the connection reset, the 404), and then by
// the write-back watcher, whose CfHydratePlaceholder call failed because of
// it and can only say "The cloud operation was unsuccessful" (Deck #686). The
// provider's report always comes first, so the later one is the one dropped.
// It is kept, not dropped, when the provider said nothing: a request that
// stalled or was withdrawn is never reported by the stream, and then the
// watcher's report is the only word the user gets.
//
// Only download failures are deduped. A success ends the stretch, so the next
// failure is news again, and so is one after the window.
type downloadDedupe struct {
	mu     sync.Mutex
	window time.Duration
	last   map[string]time.Time // local dir | shown path -> when the last failure was recorded
	now    func() time.Time
}

func newDownloadDedupe(window time.Duration) *downloadDedupe {
	return &downloadDedupe{window: window, last: map[string]time.Time{}, now: time.Now}
}

// admit reports whether a mount report should be recorded.
func (d *downloadDedupe) admit(kind, localDir, shown string, err error) bool {
	if kind != "download" {
		return true
	}
	key := localDir + "|" + shown
	d.mu.Lock()
	defer d.mu.Unlock()
	if err == nil {
		delete(d.last, key)
		return true
	}
	now := d.now()
	if at, ok := d.last[key]; ok && now.Sub(at) < d.window {
		return false
	}
	d.last[key] = now
	return true
}
