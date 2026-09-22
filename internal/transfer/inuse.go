package transfer

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// InUseError is an upload put off because the file is still open to a program
// that was caught writing to it during an earlier upload (Outlook keeps an
// attached .pst like that). Reading it again would only send another torn
// copy. It wraps the operating system's sharing violation, so code that
// already treats that as "busy, try later" keeps working.
type InUseError struct {
	Path string
	Err  error
}

func (e *InUseError) Error() string {
	return fmt.Sprintf("%s is open in another program that keeps writing to it, it will upload once that program closes it", filepath.Base(e.Path))
}

func (e *InUseError) Unwrap() error { return e.Err }

// ChangedError is an upload refused because the file changed while it was
// being read for sending (Deck #691). InPlace says a program wrote into the
// file itself, as Outlook does to a .pst, rather than an editor saving a new
// file over it: only the first marks the file as one to wait on.
type ChangedError struct {
	Path    string
	InPlace bool
}

func (e *ChangedError) Error() string {
	return fmt.Sprintf("%s changed while it was being read", filepath.Base(e.Path))
}

// Files caught changing mid-upload. Most programs, Word and Excel included,
// hold a document open to write but only write when saving, so an upload of an
// open document is normally fine and must stay allowed. A file that changed
// under an upload is different: until its writer lets go, every attempt reads
// and sends the whole file for nothing. Those are checked for a writer first.
// Kept in memory; a restart costs at most one more attempt.
var busyWriters = struct {
	sync.Mutex
	m map[string]struct{}
}{m: make(map[string]struct{})}

func busyKey(p string) string {
	p = filepath.Clean(p)
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}

func markBusyWriter(p string) {
	busyWriters.Lock()
	busyWriters.m[busyKey(p)] = struct{}{}
	busyWriters.Unlock()
}

func clearBusyWriter(p string) {
	busyWriters.Lock()
	delete(busyWriters.m, busyKey(p))
	busyWriters.Unlock()
}

func isBusyWriter(p string) bool {
	busyWriters.Lock()
	defer busyWriters.Unlock()
	_, ok := busyWriters.m[busyKey(p)]
	return ok
}

// alwaysWaitForWriter reports whether localPath is an Outlook data file,
// which never uploads while a program holds it open to write. Outlook writes
// to an attached .pst in bursts, not constantly, so a burst landing BETWEEN
// two uploads is never caught mid-read and the busy-writer mark below is never
// set: every burst then sent the whole file again, gigabytes at a time, for as
// long as Outlook stayed open (Deck #634). Nothing is lost by waiting: it
// uploads once Outlook lets go, and awaitClosed notices that within 30s.
func alwaysWaitForWriter(localPath string) bool {
	switch strings.ToLower(filepath.Ext(localPath)) {
	case ".pst", ".ost":
		return true
	}
	return false
}

// UploadDeferred reports, as an InUseError, whether an upload of localPath
// would be put off right now: a program holds it open to write, and it is
// either an Outlook data file or was caught changing under an earlier upload.
// Callers that change the server before uploading (on-demand mode sets a
// conflicting server copy aside) ask first.
func UploadDeferred(localPath string) error {
	if !alwaysWaitForWriter(localPath) && !isBusyWriter(localPath) {
		return nil
	}
	if err := writerPresent(localPath); err != nil {
		return &InUseError{Path: localPath, Err: err}
	}
	return nil
}

// statShared stats path through an open handle. On Windows that fixes the
// file's identity at the time of the call, where os.Stat looks it up from the
// path only when compared, by which time a replaced file compares as itself.
func statShared(path string) (os.FileInfo, error) {
	f, err := openShared(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Stat()
}
