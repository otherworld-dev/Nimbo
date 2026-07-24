// Package applog configures the application's logging: it sends slog output to
// both stderr (useful in a console) and a size-rotated log file (so a windowless
// build still leaves a diagnosable trail). The level is adjustable at runtime.
package applog

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

// maxSize is the log size that triggers rotation to a single ".1" backup.
const maxSize = 5 << 20 // 5 MiB

var level = new(slog.LevelVar)

// Level returns the dynamic level var so callers can change verbosity at runtime.
func Level() *slog.LevelVar { return level }

// SetVerbose switches between debug and info logging.
func SetVerbose(v bool) {
	if v {
		level.Set(slog.LevelDebug)
	} else {
		level.Set(slog.LevelInfo)
	}
}

// Setup points the default slog logger at stderr + the rotating file at path.
// On any file error it falls back to stderr-only and returns the error.
func Setup(path string, verbose bool) error {
	SetVerbose(verbose)
	opts := &slog.HandlerOptions{Level: level}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, opts)))
		return err
	}
	rotateIfLarge(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, opts)))
		return err
	}
	// File first so it always receives output even if stderr is invalid (the
	// windowless build has no real console); a failing stderr write won't lose
	// the file write.
	w := io.MultiWriter(f, os.Stderr)
	slog.SetDefault(slog.New(slog.NewTextHandler(w, opts)))
	return nil
}

// rotateIfLarge renames an oversized log to "<path>.1", discarding any previous
// backup, so the active log stays bounded.
func rotateIfLarge(path string) {
	fi, err := os.Stat(path)
	if err != nil || fi.Size() < maxSize {
		return
	}
	_ = os.Remove(path + ".1")
	_ = os.Rename(path, path+".1")
}
