package main

import (
	"bytes"
	"image/png"
	"testing"
)

// The tray follows the flyout's composed status line (Deck #988), so it never
// says idle while the flyout says "Checking folders…". Items that need the
// user (can't sync, conflicts, no longer shared) put it in a warning state, so
// they can't go unnoticed with the flyout closed (Deck #984).
func TestTrayStateFor(t *testing.T) {
	for state, want := range map[string]string{
		"idle":      "idle",
		"busy":      "sync",
		"attention": "warning",
		"offline":   "error",
		"paused":    "paused",
		"":          "idle",
	} {
		if got := trayStateFor(state); got != want {
			t.Errorf("trayStateFor(%q) = %q, want %q", state, got, want)
		}
	}
}

// The warning badge is amber, not the error red.
func TestTrayIconWarningIsAmber(t *testing.T) {
	img, err := png.Decode(bytes.NewReader(trayIcon("warning", 0, false)))
	if err != nil {
		t.Fatal(err)
	}
	// Left of the "!" stroke, inside the badge disc.
	r, g, b, _ := img.At(21, 24).RGBA()
	if c := (trayAmber); uint8(r>>8) != c.R || uint8(g>>8) != c.G || uint8(b>>8) != c.B {
		t.Errorf("badge pixel = %02x%02x%02x, want amber %02x%02x%02x", r>>8, g>>8, b>>8, c.R, c.G, c.B)
	}
}
