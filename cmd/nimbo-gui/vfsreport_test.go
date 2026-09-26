package main

import (
	"errors"
	"testing"
	"time"

	"github.com/otherworld/nimbo/internal/engine"
)

// The write-back watcher reports LOCAL names, but hydration reports the
// placeholder identity, which is the RAW server name: for a disguised file
// that is ".htaccess.nimboesc" against the watcher's ".htaccess". The
// recorder keys unresolved errors by path, so a failed hydration recorded
// under the raw name could never be cleared by a later upload recorded under
// the local one, and the Status window showed it forever (Deck #554, item 2).
// Everything a mount reports goes through one decode on the way to the feed.
func TestVFSDisplayPathShowsTheLocalName(t *testing.T) {
	esc := engine.NewEscaper(engine.NewForbidden(nil, nil, nil, nil, nil), []string{".htaccess"}, "")
	for in, want := range map[string]string{
		"web/.htaccess.nimboesc": "web/.htaccess", // a raw identity
		"web/.htaccess":          "web/.htaccess", // already the local name
		"notes.txt":              "notes.txt",
		"photo.nimboesc":         "photo.nimboesc", // a genuine name that merely ends in the marker
	} {
		if got := vfsDisplayPath(esc, in); got != want {
			t.Errorf("vfsDisplayPath(%q) = %q, want %q", in, got, want)
		}
	}
	if got := vfsDisplayPath(nil, "web/.htaccess.nimboesc"); got != "web/.htaccess.nimboesc" {
		t.Errorf("no escaper: got %q, want the name untouched", got)
	}
}

// Deck #686 (4). A pinned file that fails to download was recorded twice: the
// provider's stream reports the real error (the connection reset, the 404)
// and then the watcher, whose CfHydratePlaceholder call fails because of it,
// reports "The cloud operation was unsuccessful" for the same file. The first
// is the useful one and always arrives first, so a second download failure
// for the same file inside the window is dropped.
func TestDownloadFailureDedupeDropsTheSecondReportOfOneFailure(t *testing.T) {
	now := time.Unix(1000, 0)
	d := newDownloadDedupe(30 * time.Second)
	d.now = func() time.Time { return now }
	boom := errors.New("connection reset")

	if !d.admit("download", "E:/N", "doc.pdf", boom) {
		t.Fatal("the first failure was dropped")
	}
	now = now.Add(2 * time.Second)
	if d.admit("download", "E:/N", "doc.pdf", errors.New("The cloud operation was unsuccessful")) {
		t.Fatal("the watcher's report of the same failure was recorded as well")
	}
	if !d.admit("download", "E:/N", "other.pdf", boom) {
		t.Error("a failure on another file was dropped")
	}
	if !d.admit("download", "E:/Other", "doc.pdf", boom) {
		t.Error("a failure in another folder was dropped")
	}
	if !d.admit("upload", "E:/N", "doc.pdf", boom) {
		t.Error("an upload failure was dropped: only downloads are deduped")
	}
}

// A later attempt, after the window or after a success, is news again.
func TestDownloadFailureDedupeAdmitsALaterAttempt(t *testing.T) {
	now := time.Unix(1000, 0)
	d := newDownloadDedupe(30 * time.Second)
	d.now = func() time.Time { return now }
	boom := errors.New("connection reset")

	d.admit("download", "E:/N", "doc.pdf", boom)
	now = now.Add(31 * time.Second)
	if !d.admit("download", "E:/N", "doc.pdf", boom) {
		t.Fatal("a failure after the window was dropped")
	}
	if !d.admit("download", "E:/N", "doc.pdf", nil) {
		t.Fatal("a success was dropped")
	}
	if !d.admit("download", "E:/N", "doc.pdf", boom) {
		t.Fatal("a failure straight after a success was dropped")
	}
}
