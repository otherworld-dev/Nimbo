package main

import (
	"context"
	"log/slog"
	"strings"
)

// wailsLogger is the logger handed to Wails: Nimbo's own, with Wails' lines
// marked src=wails. Call it after the log is set up.
func wailsLogger() *slog.Logger {
	return slog.New(wailsHandler{slog.Default().Handler()}).With("src", "wails")
}

// wailsHandler keeps Wails' noise out of Nimbo's log, even with verbose logging
// on: its DEBUG output is five lines for every call the UI makes into Go (the
// flyout makes one every 250 ms during a scan), and its asset server logs every
// file a window loads at INFO. What's left is start-up info, WebView2 recovery
// and errors.
type wailsHandler struct{ slog.Handler }

func (h wailsHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= slog.LevelInfo && h.Handler.Enabled(ctx, l)
}

func (h wailsHandler) Handle(ctx context.Context, r slog.Record) error {
	if strings.HasPrefix(r.Message, "[AssetFileServerFS] ") {
		return nil
	}
	return h.Handler.Handle(ctx, r)
}

func (h wailsHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return wailsHandler{h.Handler.WithAttrs(attrs)}
}

func (h wailsHandler) WithGroup(name string) slog.Handler {
	return wailsHandler{h.Handler.WithGroup(name)}
}
