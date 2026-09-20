package atomicfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// fileLocks serialises this process's writes to any one file, keyed by path.
// Unique temp names (below) stop two writers destroying each other's temp file,
// but they don't make the commit safe on their own: Windows fails a rename onto
// a target another rename is replacing right now with "Access is denied", so
// concurrent savers would still error out. Holding this across a whole
// read-modify-write is also what stops one caller losing another's update.
//
// Within this process only: it cannot coordinate the GUI with a separate CLI
// run. Those are protected only by the atomic replace, so one of them wins
// whole and neither sees a torn file.
var fileLocks sync.Map // key(path) -> *sync.Mutex

// key normalises a path for the lock map. Lowercasing is right for Windows and
// merely over-shares one mutex between case-variant paths elsewhere.
func key(path string) string { return strings.ToLower(filepath.Clean(path)) }

// Mutex returns the lock guarding writes to path, for callers that need the
// read-modify-write around a Write serialised and not just the write itself.
// Hold it and call WriteLocked.
func Mutex(path string) *sync.Mutex {
	mu, _ := fileLocks.LoadOrStore(key(path), &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// Write replaces path's contents in one step: a reader sees either the old file
// or the new one, never a partial write and never nothing at all.
//
// The temp file is UNIQUELY named and sits beside the target. A fixed
// "<path>.tmp" is shared by every concurrent writer, which is how a save could
// fail outright rather than merely lose a race: the first writer's rename
// consumed the temp file and the second found nothing left to rename (ENOENT on
// Linux/Android; on Windows the two collide on the open handle instead). Unique
// names mean concurrent writers still race for the target, last one wins, but
// every one of them completes. Callers that must not lose a concurrent update
// need to serialise the read-modify-write too; see Mutex.
//
// The data is flushed to disk before the commit, so what survives a crash is a
// whole file rather than an empty one with a valid name, and the parent
// directory is created if it doesn't exist yet.
func Write(path string, data []byte, perm os.FileMode) error {
	mu := Mutex(path)
	mu.Lock()
	defer mu.Unlock()
	return WriteLocked(path, data, perm)
}

// WriteLocked is Write's body, for callers already holding the file's Mutex.
func WriteLocked(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", path, err)
	}
	tmp := f.Name()
	// A no-op once the commit below has moved the file away; on every failure
	// path it is what stops unique temp names accumulating as litter.
	defer os.Remove(tmp)

	err = fillTemp(f, data, perm)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := Commit(tmp, path); err != nil {
		return fmt.Errorf("commit %s: %w", path, err)
	}
	return nil
}

// fillTemp writes data into the freshly created temp file f and flushes it. It
// does not close f; the caller does, so a close error is reported either way.
func fillTemp(f *os.File, data []byte, perm os.FileMode) error {
	// os.CreateTemp already creates the file 0600; this honours any other mode a
	// caller asks for. Windows models only the read-only bit and some
	// filesystems model none at all, so an unsupported chmod is not a failure.
	if err := f.Chmod(perm); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	// Flush before the commit: the whole point of the dance is that a crash
	// leaves either the old file or the new one, not a plausible-looking empty
	// file where the user's settings used to be.
	return f.Sync()
}
