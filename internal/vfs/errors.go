package vfs

import "errors"

// ErrHeldByLock means an upload was declined because another user holds the
// file locked on the server.
//
// It is NOT a failure. The on-demand write-back watcher has no retry anywhere —
// an upload that errors is simply dropped until the file changes again — so a
// hold reported as an error would strand the user's edit indefinitely. The
// watcher re-arms on this sentinel instead, and deliberately does not mark the
// file in-sync, because a later refresh would then dehydrate the edit away.
//
// Declared in a file with no build tag: it crosses the platform boundary (the
// GUI returns it from the upload op on every OS) while the watcher that
// recognises it is Windows-only.
var ErrHeldByLock = errors.New("upload held: file is locked by another user")
