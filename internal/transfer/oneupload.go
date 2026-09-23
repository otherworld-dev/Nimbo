package transfer

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
)

// ErrUploadInProgress is an upload refused because this process is already
// uploading the same file. It is not a failure: the upload that is running
// reports the outcome, and if that one fails the file is still changed and
// the next pass sends it.
var ErrUploadInProgress = errors.New("already being uploaded")

// Files being uploaded right now, keyed like busyWriters. Two uploads of one
// file share its chunk session (uploadIDFor is the file's path, size and
// mtime), so they overwrite each other's chunks, and the first to assemble
// finds the server a chunk short and deletes the session under the other
// (Deck #714). Sync passes are allowed to overlap, and a conflict choice
// uploads outside any pass, so this is the one place all of them meet.
var uploading = struct {
	sync.Mutex
	m map[string]struct{}
}{m: make(map[string]struct{})}

// beginUpload claims localPath for one upload, or reports that another upload
// of it is already running.
func beginUpload(localPath string) error {
	k := busyKey(localPath)
	uploading.Lock()
	defer uploading.Unlock()
	if _, ok := uploading.m[k]; ok {
		return fmt.Errorf("%s: %w", filepath.Base(localPath), ErrUploadInProgress)
	}
	uploading.m[k] = struct{}{}
	return nil
}

func endUpload(localPath string) {
	uploading.Lock()
	delete(uploading.m, busyKey(localPath))
	uploading.Unlock()
}

// Uploading reports whether an upload of localPath is running right now, for a
// caller that must not change the server ahead of one (on-demand mode sets a
// conflicting server copy aside before it uploads).
func Uploading(localPath string) bool {
	uploading.Lock()
	defer uploading.Unlock()
	_, ok := uploading.m[busyKey(localPath)]
	return ok
}
