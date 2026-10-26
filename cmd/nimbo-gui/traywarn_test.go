package main

import (
	"bytes"
	"image/png"
	"testing"
)

// Items that need the user (can't sync, conflicts, no longer shared) put the
// tray in a warning state, so they can't go unnoticed with the flyout closed
// (Deck #984). Paused, syncing and a real error still take priority.
func TestTrayStateFor(t *testing.T) {
	for _, tc := range []struct {
		paused    bool
		status    string
		attention int
		want      string
	}{
		{false, "Up to date", 0, "idle"},
		{false, "Up to date", 2, "warning"},
		{false, "Syncing…", 2, "sync"},
		{false, "Error: server unreachable", 2, "error"},
		{true, "Up to date", 2, "paused"},
	} {
		if got := trayStateFor(tc.paused, tc.status, tc.attention); got != tc.want {
			t.Errorf("trayStateFor(paused=%v, %q, %d) = %q, want %q", tc.paused, tc.status, tc.attention, got, tc.want)
		}
	}
}

func TestTrayAttentionLine(t *testing.T) {
	if got := trayAttentionLine(1); got != "1 item needs attention" {
		t.Errorf("1 -> %q", got)
	}
	if got := trayAttentionLine(3); got != "3 items need attention" {
		t.Errorf("3 -> %q", got)
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
