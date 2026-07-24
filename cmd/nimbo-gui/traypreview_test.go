package main

import (
	"os"
	"testing"
)

// TestRenderTrayPreview writes the tray states to .icon-preview for visual
// inspection. Not a real test; kept handy for icon work.
func TestRenderTrayPreview(t *testing.T) {
	if os.Getenv("NIMBO_ICON_PREVIEW") == "" {
		t.Skip("set NIMBO_ICON_PREVIEW=1 to render")
	}
	_ = os.MkdirAll("../../.icon-preview", 0o755)
	for _, st := range []string{"idle", "sync", "paused", "error"} {
		_ = os.WriteFile("../../.icon-preview/tray-"+st+".png", trayIcon(st, 3, st == "idle"), 0o644)
	}
}
