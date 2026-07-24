//go:build !windows

package watch

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/fsnotify/fsnotify"
)

// Run watches Root with fsnotify (one watch per directory, added recursively)
// and feeds changed paths into the shared loop. Used on non-Windows platforms;
// Windows uses a single recursive ReadDirectoryChangesW handle instead.
func Run(ctx context.Context, opts Options, sync SyncFunc) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()
	if err := addRecursive(w, opts.Root); err != nil {
		return err
	}

	events := make(chan string, 256)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				// A newly created directory must itself be watched.
				if ev.Op&fsnotify.Create != 0 {
					if fi, serr := os.Stat(ev.Name); serr == nil && fi.IsDir() {
						_ = addRecursive(w, ev.Name)
					}
				}
				if relevant(ev.Op) {
					select {
					case events <- ev.Name:
					case <-ctx.Done():
						return
					}
				}
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				// An error (notably inotify queue overflow) can mean dropped events,
				// so ask runLoop for a prompt full scan to recover.
				slog.Warn("watcher error; forcing a full scan", "err", err)
				select {
				case events <- overflowSignal:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return runLoop(ctx, opts, sync, events)
}

// relevant reports whether an fsnotify op should trigger a sync. Chmod-only
// events are ignored to avoid needless churn.
func relevant(op fsnotify.Op) bool {
	return op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) != 0
}

// addRecursive registers root and all of its subdirectories with the watcher.
func addRecursive(w *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries rather than aborting
		}
		if d.IsDir() {
			if err := w.Add(p); err != nil {
				slog.Debug("watch add failed", "path", p, "err", err)
			}
		}
		return nil
	})
}
