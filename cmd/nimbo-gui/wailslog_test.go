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

// Wails' asset server logs every file a window loads at INFO, five lines each
// time a window opens. That is debug detail: it must not fill the log at the
// normal level, and it must still be there with verbose logging on.
func TestWailsAssetRequestsAreDebugOnly(t *testing.T) {
	var buf bytes.Buffer
	lv := new(slog.LevelVar)
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: lv})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	l := wailsLogger()
	l.Info("[AssetFileServerFS] Handling request", "url", "/style.css")
	l.Info("webview2: rebuilding controller after browser process exit")
	got := buf.String()
	if strings.Contains(got, "Handling request") {
		t.Errorf("asset request logged at the normal level: %q", got)
	}
	if !strings.Contains(got, "level=INFO") || !strings.Contains(got, "rebuilding controller") {
		t.Errorf("Wails' other INFO lines must stay: %q", got)
	}

	buf.Reset()
	lv.Set(slog.LevelDebug)
	l.Info("[AssetFileServerFS] Handling request", "url", "/style.css")
	if got := buf.String(); !strings.Contains(got, "level=DEBUG") || !strings.Contains(got, "url=/style.css") {
		t.Errorf("with verbose logging the asset request should show at DEBUG: %q", got)
	}
}
