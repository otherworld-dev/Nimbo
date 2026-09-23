package main

import "testing"

// The tray tooltip names the app and says what it is doing, the way OneDrive's
// does, rather than the bare name (GitHub #9 follow-up).
func TestTrayTooltip(t *testing.T) {
	for _, tc := range []struct {
		status string
		paused bool
		want   string
	}{
		{"Syncing…", false, "Nimbo\nSyncing…"},
		{"Up to date", false, "Nimbo\nUp to date"},
		{"  ", false, "Nimbo"},
		{"Syncing…", true, "Nimbo\nPaused"},
	} {
		if got := trayTooltip("Nimbo", tc.status, tc.paused); got != tc.want {
			t.Errorf("trayTooltip(%q, paused=%v) = %q, want %q", tc.status, tc.paused, got, tc.want)
		}
	}
}
