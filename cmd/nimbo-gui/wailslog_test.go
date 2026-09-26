package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// Wails logs its own trouble (a WebView2 process that died and was rebuilt, a
// bound method that panicked) through the logger it is given. Without one,
// the windowsgui build sends it to a stderr nobody sees, so it has to land in
// Nimbo's log, marked as Wails'.
func TestWailsLoggerWritesToTheAppLog(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	wailsLogger().Error("webview2: rebuilding controller")

	got := buf.String()
	if !strings.Contains(got, "webview2: rebuilding controller") || !strings.Contains(got, "src=wails") {
		t.Fatalf("app log got %q, want the message tagged src=wails", got)
	}
}
