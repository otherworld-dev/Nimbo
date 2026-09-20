package config

import (
	"os"
	"sync"

	"github.com/otherworld/nimbo/internal/atomicfile"
)

// The atomic save itself lives in internal/atomicfile, which the account store
// shares; these keep the config package's own vocabulary for it.

// fileMutex returns the lock serialising this process's writes to one config
// file. Callers that must not lose a concurrent update hold it across the whole
// read-modify-write; see UpdateSettings.
func fileMutex(path string) *sync.Mutex { return atomicfile.Mutex(path) }

// writeFileAtomic replaces path's contents in one step: a reader sees either
// the old file or the new one, never a partial write and never nothing at all.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	return atomicfile.Write(path, data, perm)
}

// writeFileLocked is writeFileAtomic's body, for callers that already hold the
// file's mutex because they need the surrounding read-modify-write serialised.
func writeFileLocked(path string, data []byte, perm os.FileMode) error {
	return atomicfile.WriteLocked(path, data, perm)
}
