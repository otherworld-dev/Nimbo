//go:build !windows

package vfs

import (
	"context"
	"time"

	"github.com/otherworld/nimbo/internal/cfapi"
)

// Ops are the server-side actions the watcher performs (unused off Windows).
type Ops struct {
	Upload func(ctx context.Context, localPath, remotePath string) error
	Mkdir  func(ctx context.Context, remotePath string) error
	Delete func(ctx context.Context, remotePath string) error
	Move   func(ctx context.Context, srcRemote, dstRemote string) error
	List   func(rel string) ([]cfapi.PlaceholderInfo, error)
	Report         func(kind, remotePath string, err error)
	RecordBaseline func(remotePath, etag string)
	Baseline       func(remotePath string) (string, bool)
	RecordFileID   func(remotePath, fileid string)
	FileID         func(remotePath string) (string, bool)
	DropFileID     func(remotePath string)
	Log            func(format string, args ...any)
}

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
