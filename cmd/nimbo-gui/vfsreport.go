package main

import "github.com/otherworld/nimbo/internal/engine"

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
