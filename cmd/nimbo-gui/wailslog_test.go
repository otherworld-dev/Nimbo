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

// Wails writes five DEBUG lines for every call the UI makes into Go, and the
// flyout makes one every 250 ms during a scan: with verbose logging on that
// buried Nimbo's own lines. Its asset server also logs every file a window
// loads at INFO. None of it may reach the log, even with verbose logging on,
// while Wails' real INFO lines (start-up info, WebView2 recovery) still do.
func TestWailsNoiseStaysOutOfTheLog(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	l := wailsLogger()
	l.Debug("Binding call started:", "method", "main.App.NeedsLogin")
	l.Debug("handleWebViewRequest: Processing request", "url", "http://wails.localhost/wails/runtime")
	l.Info("[AssetFileServerFS] Handling request", "url", "/style.css")
	l.Info("webview2: rebuilding controller after browser process exit")

	got := buf.String()
	for _, noise := range []string{"Binding call", "handleWebViewRequest", "Handling request"} {
		if strings.Contains(got, noise) {
			t.Errorf("%q reached the log with verbose logging on: %q", noise, got)
		}
	}
	if !strings.Contains(got, "level=INFO") || !strings.Contains(got, "rebuilding controller") {
		t.Errorf("Wails' own INFO lines must stay: %q", got)
	}
}
