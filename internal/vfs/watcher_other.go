//go:build !windows

package vfs

import (
	"context"
	"time"
)

// Watcher is a no-op outside Windows (on-demand files are Windows-only).
type Watcher struct{}

// New returns a no-op watcher off Windows.
func New(context.Context, string, string, time.Duration, Ops) (*Watcher, error) {
	return &Watcher{}, nil
}

// Close does nothing off Windows.
func (*Watcher) Close() {}

// Poke does nothing off Windows.
func (*Watcher) Poke() {}

// PauseChanged does nothing off Windows.
func (*Watcher) PauseChanged() {}

// NotifyRenamed does nothing off Windows.
func (*Watcher) NotifyRenamed(string, string) {}
