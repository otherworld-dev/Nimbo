// Package atomicfile commits a fully written temp file onto its final name.
//
// The commit is a rename, so a reader sees either the old file or the new one
// and never a half-written one. Where the rename cannot work at all the file is
// rewritten in place instead: losing that guarantee beats losing the write,
// which is what used to happen (GitHub issue #8).
package atomicfile

import (
	"errors"
	"io"
	"os"
)

// rename is a seam for the tests. Windows cannot be asked to fail a
// same-directory rename on demand, and that failure is this package's subject.
var rename = os.Rename

// Commit moves a fully written temp file onto path, replacing whatever is
// there. The temp file is gone afterwards either way.
//
// Normally that is one atomic rename. When the rename reports a cross-volume
// move the temp file's bytes are written over the destination instead. A
// same-directory rename can be cross-volume without the paths looking odd at
// all: a packaged (MSIX) build's %AppData% is virtualised into the package's
// LocalCache under %LocalAppData%, so the temp file and its final name sit on
// different disks on any machine that keeps those two on different drives.
// Issue #8 was a first sign-in failing outright on exactly that, having written
// the temp file perfectly well.
//
// The fallback is not atomic: an ill-timed crash can leave the destination
// truncated. It only runs where the atomic route is impossible, and there the
// alternative is a write that never lands at all.
func Commit(tmp, path string) error {
	err := rename(tmp, path)
	if err == nil || !isCrossVolume(err) {
		return err
	}
	if err := writeThrough(tmp, path); err != nil {
		return err
	}
	// The bytes are safely at path; a temp file we then fail to tidy away is
	// litter, not a failed save.
	_ = os.Remove(tmp)
	return nil
}

// writeThrough copies the temp file's contents over path, carrying the temp
// file's mode with them so the destination ends up as the rename would have
// left it.
func writeThrough(tmp, path string) error {
	src, err := os.Open(tmp)
	if err != nil {
		return err
	}
	defer src.Close()
	fi, err := src.Stat()
	if err != nil {
		return err
	}
	perm := fi.Mode().Perm()

	dst, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	// O_CREATE's mode applies only to a file this call creates, so an existing
	// destination keeps whatever mode it had; the rename would have replaced
	// it. Windows models only the read-only bit and some filesystems model
	// none, so an unsupported chmod is not a failure.
	err = dst.Chmod(perm)
	if errors.Is(err, errors.ErrUnsupported) {
		err = nil
	}
	if err == nil {
		_, err = io.Copy(dst, src)
	}
	// Flush before returning, for the same reason the caller wrote a temp file
	// at all: what survives a crash should be the new contents, not a
	// plausible-looking empty file where the settings used to be.
	if err == nil {
		err = dst.Sync()
	}
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	return err
}
