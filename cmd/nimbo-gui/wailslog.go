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

// wailsHandler demotes Wails' asset-server lines to DEBUG: it logs every file a
// window loads at INFO, which buries the lines that matter.
type wailsHandler struct{ slog.Handler }

func (h wailsHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level == slog.LevelInfo && strings.HasPrefix(r.Message, "[AssetFileServerFS] ") {
		if !h.Handler.Enabled(ctx, slog.LevelDebug) {
			return nil
		}
		r.Level = slog.LevelDebug
	}
	return h.Handler.Handle(ctx, r)
}

func (h wailsHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return wailsHandler{h.Handler.WithAttrs(attrs)}
}

func (h wailsHandler) WithGroup(name string) slog.Handler {
	return wailsHandler{h.Handler.WithGroup(name)}
}
