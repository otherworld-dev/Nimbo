package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// HeldLock is a files_lock lock that THIS install currently holds on the server.
//
// It is persisted because a default Nextcloud never expires a lock: if Nimbo
// takes one and the process dies, nothing on the server will ever clear it, and
// no other user can either (UNLOCK by a non-owner returns 423). This registry is
// the only way a later run can find its own strays and release them.
type HeldLock struct {
	Account    string    `json:"account"`    // login name that took it — one file, several accounts
	RemotePath string    `json:"remotePath"` // files-root-relative, as passed to Lock/Unlock
	Token      string    `json:"token"`      // nc:lock-token, for diagnostics
	Taken      time.Time `json:"taken"`
	// LockFile is the editor lock file ("~$Report.docx", ".~lock.x.odt#")
	// whose appearing took this lock, absolute; empty for a lock taken any
	// other way. Its disappearance releases the lock even when the close
	// event itself was missed (Deck #722).
	LockFile string `json:"lockFile,omitempty"`
	// Released marks a note, not a lock: the lock was given back when Nimbo
	// exited while its document was still open, so the next start locks it
	// again if LockFile is still there, and otherwise forgets it.
	Released bool `json:"released,omitempty"`
}

// LocksFile is the path to the registry of locks we hold. It lives in Config,
// not Data, for the same reason SyncHistoryFile does: it must survive a reset of
// the Data-side sync database, or a state reset would strand every lock we hold.
//
// Deliberately NOT account-scoped: one file lists every account's locks, so a
// single sweep at startup can see them all, and each entry carries its own
// Account so a sweep only ever releases its own.
func (d Dirs) LocksFile() string {
	return filepath.Join(d.Config, "held-locks.json")
}

// LoadHeldLocks reads the registry. A missing OR CORRUPT file yields an empty
// list and no error: failing to start because a diagnostic side-file is damaged
// would be far worse than losing track of a lock, and the entries are rewritten
// from memory on the next change anyway.
func (d Dirs) LoadHeldLocks() ([]HeldLock, error) {
	data, err := os.ReadFile(d.LocksFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, nil
	}
	var out []HeldLock
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, nil
	}
	return out, nil
}

// SynthFilesFile records the editor lock files NIMBO created, so a later run can
// clean up after a crash.
//
// It has to be a record rather than a pattern match: a synthesised "~$Report.docx"
// is byte-indistinguishable from one a real Word session left behind, and
// deleting somebody's genuine owner file would break their Office locking. If
// it is not in this list, we did not make it and we do not touch it.
func (d Dirs) SynthFilesFile() string {
	return filepath.Join(d.Config, "synth-lockfiles.json")
}

// LoadSynthFiles reads the list. Missing or corrupt yields empty, never an error
// — see LoadHeldLocks for why.
func (d Dirs) LoadSynthFiles() []string {
	data, err := os.ReadFile(d.SynthFilesFile())
	if err != nil {
		return nil
	}
	var out []string
	if err := json.Unmarshal(data, &out); err != nil {
		return nil
	}
	return out
}

// SaveSynthFiles atomically rewrites the list.
func (d Dirs) SaveSynthFiles(paths []string) error {
	if paths == nil {
		paths = []string{}
	}
	data, err := json.MarshalIndent(paths, "", "  ")
	if err != nil {
		return err
	}
	tmp := d.SynthFilesFile() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write synth lock files: %w", err)
	}
	if err := os.Rename(tmp, d.SynthFilesFile()); err != nil {
		return fmt.Errorf("commit synth lock files: %w", err)
	}
	return nil
}

// SaveHeldLocks atomically rewrites the registry (tmp + rename, like SavePairs).
func (d Dirs) SaveHeldLocks(locks []HeldLock) error {
	if locks == nil {
		locks = []HeldLock{}
	}
	data, err := json.MarshalIndent(locks, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(d.LocksFile(), data, 0o600); err != nil {
		return fmt.Errorf("save held locks: %w", err)
	}
	return nil
}
