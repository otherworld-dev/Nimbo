package vfs

import (
	"context"

	"github.com/otherworld/nimbo/internal/cfapi"
)

// Ops are the server-side actions the watcher performs (backed by the engine).
type Ops struct {
	Upload func(ctx context.Context, localPath, remotePath string) error
	Mkdir  func(ctx context.Context, remotePath string) error
	Delete func(ctx context.Context, remotePath string) error
	Move   func(ctx context.Context, srcRemote, dstRemote string) error
	// List returns the children of a sync-root-relative directory ("" = root)
	// as placeholders, for down-sync reconciliation. A non-nil error means the
	// listing is unknown (e.g. a network failure) and must NOT be treated as an
	// empty directory.
	List func(rel string) ([]cfapi.PlaceholderInfo, error)
	// CheckList lists a server folder for the delete guard, sync-root-relative
	// like List, but completely and quietly: end-to-end encrypted folders are
	// included (Encrypted set) rather than skipped, nothing is recorded as a
	// side effect, and a listing that cannot finish returns an error (a
	// transport.ErrNotFound one when the folder is not there). Nil makes the
	// guard use List.
	CheckList func(rel string) ([]cfapi.PlaceholderInfo, error)
	// Stat reports whether a RAW server path currently exists — used to tell a
	// lost MOVE response (the server applied the rename) from a real failure.
	// Nil disables that detection.
	Stat func(remote string) (bool, error)
	// Report surfaces a completed operation for the activity feed / error toasts
	// (kind e.g. "upload"/"delete-remote"/"move"/"delete-local"; err non-nil on
	// failure).
	Report func(kind, remotePath string, err error)
	// RecordBaseline records the server ETag a placeholder now mirrors (the
	// conflict baseline), set when we create/refresh an in-sync placeholder.
	RecordBaseline func(remotePath, etag string)
	// Baseline returns the recorded ETag for a remote path (ok=false if none).
	// Down-sync uses it to detect server-side edits reliably (any content change
	// alters the ETag), rather than relying only on the size/mtime heuristic.
	Baseline func(remotePath string) (string, bool)
	// RecordContent / Content keep the content-version key
	// (transport.ContentKey) of the server version a placeholder mirrors,
	// alongside its ETag baseline. A new ETag with the same key is a
	// metadata-only bump — files_lock changes the ETag on every lock and
	// unlock — not an edit. "" = unknown, and then the ETag alone decides.
	// RecordContent takes a batch (one store write per directory, like
	// RecordBaselines); an empty key clears that path's entry, so a key never
	// outlives the version it was recorded for. Nil disables it.
	RecordContent func(keyByRemotePath map[string]string)
	Content       func(remotePath string) string
	// EditorLockFiles receives the absolute paths of editor lock files
	// (Office "~$…", LibreOffice ".~lock.…#") that were created or removed,
	// straight from the change journal and before they are skipped, so the
	// engine can lock a document on the server while it is open (Deck #721).
	// Called on its own goroutine. Nil disables it.
	EditorLockFiles func(absPaths []string)
	// BeforeReplace runs just before the watcher dehydrates a downloaded file
	// (a refresh after a server edit, or "Free up space"). The lockout holds a
	// deny-write handle on a file a colleague has open, and while it is held
	// the dehydrate fails and the file is left wearing pending arrows
	// (measured on the VM, 2026-09-23); this is where it lets go. Nil
	// disables it.
	BeforeReplace func(absPath string)
	// ForgetBaseline drops the recorded ETag for a remote path the server no
	// longer holds under that name (the source of a MOVE). Nil disables it.
	ForgetBaseline func(remotePath string)
	// MoveBaselines carries several baselines across a move at once (pairs of
	// src, dst RAW server paths): each dst takes src's recorded ETag and src is
	// dropped. RecordBaselines records several baselines at once.
	//
	// Both exist because the store behind these hooks persists its ENTIRE file
	// per call: a moved directory of 1,000 files cost 2,000 whole-file rewrites
	// of a multi-megabyte JSON, synchronously, inside the move. Nil falls back
	// to the one-item hooks above, so a caller wiring only those still works.
	MoveBaselines   func(pairs [][2]string)
	RecordBaselines func(etagByRemotePath map[string]string)
	// RecordFileID / FileID / DropFileID persist the server oc:fileid per remote
	// path so down-sync can recognise a server rename (old path gone, new path
	// with the same fileid) and move the placeholder instead of delete+recreate.
	RecordFileID func(remotePath, fileid string)
	FileID       func(remotePath string) (string, bool)
	DropFileID   func(remotePath string)
	// MountRoot reports whether a RAW server path was, when last listed, the
	// ROOT of a share received from someone else or of a mount (Deck #557).
	// Such a path vanishing from a listing means it was detached from the
	// account, not deleted. Nil disables the distinction: every vanish is a
	// deletion, as before.
	MountRoot func(remotePath string) bool
	// Detached takes over a vanished share's salvaged local copy at localPath
	// (plain files only by then — see Watcher.salvage), keyed by its RAW server
	// path: the caller parks it outside the cloud folder and tells the user.
	// An error leaves the copy where it is for the next pass to retry.
	Detached func(localPath, remotePath string) error
	// Forget is told, once a vanished share's copy has left the mount (parked)
	// or was removed as holding nothing, that whatever is recorded under
	// remotePath — etag baselines, file ids, the root mark — no longer applies.
	// Seen live: the stub-only share's stale entries would have let the
	// mount-state heal read a later folder of the same name as server content,
	// and its stale root mark would then have parked that folder as "unshared".
	Forget func(remotePath string)
	// Encode maps a LOCAL (user-visible) rel path to the RAW path the server
	// stores it under, for disguised file types: Nextcloud forbids ".htaccess",
	// so it lives on the server as ".htaccess.nimboesc". Decode is the inverse and
	// is used only for display. Nil means escaping is off and names pass through.
	//
	// Escaping applies to FILE basenames only — directories are never escaped, so
	// callers must not encode a directory path (see serverFor).
	//
	// These are called PER OPERATION and must never be captured by the caller: the
	// engine swaps its escaper (an atomic pointer) whenever the user toggles a
	// type in Settings, and a mount outlives that.
	Encode func(rel string) string
	Decode func(rel string) string
	// Paused reports whether the user has paused syncing (Pause in the tray,
	// "pause for", quiet hours). While it does, no file upload starts, a
	// running one is stopped to wait with the rest, and pinned files are not
	// downloaded (Deck #723). Opening a file still downloads it: that is the
	// provider's hydrate callback, not the watcher, and refusing it would
	// hang the app doing the opening. Folder creates, deletes and moves are
	// not held either — they carry no file data, and holding them would
	// leave the server's names out of step with this PC for the whole pause,
	// for down-sync to "correct". Nil means never paused. The caller calls
	// PauseChanged whenever the answer changes.
	Paused func() bool
	Log    func(format string, args ...any)
}
