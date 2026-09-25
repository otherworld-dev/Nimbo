package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// SeenLock is a lock SOMEONE ELSE holds on one of the account's files, as last
// seen in a listing.
//
// It is persisted so the "In use" list, the toasts' already-announced state and
// the lockout survive a restart (GitHub #7). Only a listing can tell us about a
// lock, and a folder whose ETag has not changed is never listed again, so an
// existing lock would otherwise stay invisible until something in its folder
// changed. A lock taken or released while Nimbo was closed is still picked up:
// every LOCK and UNLOCK changes the folder's ETag, so that folder is re-listed.
type SeenLock struct {
	LocalDir     string    `json:"localDir"` // the sync pair's (or mount's) local root
	Path         string    `json:"path"`     // relative to LocalDir, "/"-separated
	Owner        string    `json:"owner,omitempty"`
	OwnerDisplay string    `json:"ownerDisplay,omitempty"`
	AppName      string    `json:"appName,omitempty"`
	OwnerType    int       `json:"ownerType"`
	Since        time.Time `json:"since,omitempty"`
}

// SeenLocksFile is the per-account list of other people's locks, scoped like
// PairsFile.
func (d Dirs) SeenLocksFile() string {
	if d.acct != "" {
		return filepath.Join(d.Config, "seen-locks-"+d.acct+".json")
	}
	return filepath.Join(d.Config, "seen-locks.json")
}

// LoadSeenLocks reads the list. Missing or corrupt yields empty and no error:
// the list is only a head start, the next listing of each folder is the truth.
func (d Dirs) LoadSeenLocks() []SeenLock {
	data, err := os.ReadFile(d.SeenLocksFile())
	if err != nil {
		return nil
	}
	var out []SeenLock
	if err := json.Unmarshal(data, &out); err != nil {
		return nil
	}
	return out
}

// SaveSeenLocks atomically rewrites the list.
func (d Dirs) SaveSeenLocks(locks []SeenLock) error {
	if locks == nil {
		locks = []SeenLock{}
	}
	data, err := json.MarshalIndent(locks, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(d.SeenLocksFile(), data, 0o600); err != nil {
		return fmt.Errorf("save seen locks: %w", err)
	}
	return nil
}
