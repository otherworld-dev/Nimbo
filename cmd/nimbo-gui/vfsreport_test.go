package main

import (
	"testing"

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
