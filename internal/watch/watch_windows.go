//go:build windows

package watch

import (
	"context"
	"log/slog"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// notifyFlags are the change categories we react to: names (create/rename/
// delete), sizes and last-write (content edits).
const notifyFlags = windows.FILE_NOTIFY_CHANGE_FILE_NAME |
	windows.FILE_NOTIFY_CHANGE_DIR_NAME |
	windows.FILE_NOTIFY_CHANGE_SIZE |
	windows.FILE_NOTIFY_CHANGE_LAST_WRITE

// Run watches Root with a single recursive ReadDirectoryChangesW handle — one OS
// object for the entire subtree, regardless of how many folders it contains —
// and feeds changed absolute paths into the shared loop. This is what lets the
// watcher scale to very large trees (vs. fsnotify's one-watch-per-directory).
func Run(ctx context.Context, opts Options, sync SyncFunc) error {
	pathW, err := windows.UTF16PtrFromString(opts.Root)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(pathW,
		windows.FILE_LIST_DIRECTORY,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	events := make(chan string, 1024)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		buf := make([]byte, 64*1024)
		for {
			var n uint32
			err := windows.ReadDirectoryChanges(h, &buf[0], uint32(len(buf)),
				true, // watch the whole subtree with this one handle
				notifyFlags, &n, nil, 0)
			if err != nil || ctx.Err() != nil {
				return // handle cancelled/closed or errored — poll remains the fallback
			}
			if n == 0 {
				// Buffer overflowed (a burst larger than 64 KB of change records):
				// the specific paths are lost, so ask runLoop for a prompt full scan.
				slog.Debug("watch buffer overflow; forcing a full scan")
				select {
				case events <- overflowSignal:
				case <-ctx.Done():
					return
				}
				continue
			}
			for _, name := range parseNotifyNames(buf[:n]) {
				select {
				case events <- filepath.Join(opts.Root, name):
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	err = runLoop(ctx, opts, sync, events)

	// Clean shutdown: cancel the in-flight ReadDirectoryChanges, wait for the
	// reader goroutine to actually exit, THEN close the handle. Closing a handle
	// that still has a ReadDirectoryChanges pending can block/deadlock (which the
	// race detector reliably surfaces) and leaks the goroutine + handle.
	_ = windows.CancelIoEx(h, nil)
	<-readerDone
	_ = windows.CloseHandle(h)
	return err
}

// parseNotifyNames extracts the entry names from a FILE_NOTIFY_INFORMATION
// buffer. We don't care which action each record is (add/modify/delete/rename) —
// the sync layer reconciles the affected subtree from the path alone — so both
// halves of a rename are reported as touched.
func parseNotifyNames(b []byte) []string {
	var names []string
	for off := 0; off+12 <= len(b); {
		next := *(*uint32)(unsafe.Pointer(&b[off]))
		nameLen := *(*uint32)(unsafe.Pointer(&b[off+8])) // bytes
		nameStart := off + 12
		if nameStart+int(nameLen) > len(b) {
			break
		}
		name := windows.UTF16ToString(unsafe.Slice((*uint16)(unsafe.Pointer(&b[nameStart])), nameLen/2))
		if name != "" {
			names = append(names, name)
		}
		if next == 0 {
			break
		}
		off += int(next)
	}
	return names
}
