package main

import "testing"

// The tray tooltip names the app and says what it is doing, the way OneDrive's
// does, rather than the bare name (GitHub #9 follow-up). The second line is
// the flyout's status line (Deck #988).
func TestTrayTooltip(t *testing.T) {
	for _, tc := range []struct {
		line string
		want string
	}{
		{"Uploading Survey-North.tif", "Nimbo\nUploading Survey-North.tif"},
		{"Up to date", "Nimbo\nUp to date"},
		{"  ", "Nimbo"},
	} {
		if got := trayTooltip("Nimbo", tc.line); got != tc.want {
			t.Errorf("trayTooltip(%q) = %q, want %q", tc.line, got, tc.want)
		}
	}
}
