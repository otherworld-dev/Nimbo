package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// SyncPair binds a local directory to a remote folder for continuous sync. It is
// the persisted form of agent.Pair; the agent package imports this rather than
// the reverse to avoid a dependency cycle.
type SyncPair struct {
	LocalDir   string   `json:"localDir"`
	RemoteRoot string   `json:"remoteRoot"`
	Excludes   []string `json:"excludes,omitempty"` // pair-specific ignore patterns (selective sync)
}

// PairsFile is the path to the persisted sync-pair list. When the Dirs is
// bound to an account (WithAccount) the list is per-account, so each account
// keeps its own folder setup; unscoped it is the legacy single-account file.
func (d Dirs) PairsFile() string {
	if d.acct != "" {
		return filepath.Join(d.Config, "pairs-"+d.acct+".json")
	}
	return filepath.Join(d.Config, "pairs.json")
}

// VFSETagsFile is the on-demand collection-ETag / fileid cache, scoped like
// PairsFile so two accounts' remote paths can't cross-contaminate.
func (d Dirs) VFSETagsFile() string {
	if d.acct != "" {
		return filepath.Join(d.Config, "vfs-etags-"+d.acct+".json")
	}
	return filepath.Join(d.Config, "vfs-etags.json")
}

// VFSFileIDsFile is the on-demand remote-path -> oc:fileid map (rename
// detection), scoped like PairsFile.
func (d Dirs) VFSFileIDsFile() string {
	if d.acct != "" {
		return filepath.Join(d.Config, "vfs-fileids-"+d.acct+".json")
	}
	return filepath.Join(d.Config, "vfs-fileids.json")
}

// MigratePairs adopts the legacy unscoped files into this account's scoped
// ones — a one-time rename for installs that predate multi-account. It only
// acts when the Dirs is account-bound, the scoped file doesn't exist yet, and
// the legacy file does.
func (d Dirs) MigratePairs() {
	if d.acct == "" {
		return
	}
	for _, m := range [][2]string{
		{filepath.Join(d.Config, "pairs.json"), d.PairsFile()},
		{filepath.Join(d.Config, "vfs-etags.json"), d.VFSETagsFile()},
		{filepath.Join(d.Config, "vfs-fileids.json"), d.VFSFileIDsFile()},
	} {
		if _, err := os.Stat(m[1]); !errors.Is(err, os.ErrNotExist) {
			continue // scoped file already exists
		}
		if _, err := os.Stat(m[0]); err == nil {
			_ = os.Rename(m[0], m[1])
		}
	}
}

// LoadPairs reads the configured sync pairs. A missing file yields an empty list.
func (d Dirs) LoadPairs() ([]SyncPair, error) {
	data, err := os.ReadFile(d.PairsFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read pairs: %w", err)
	}
	var pairs []SyncPair
	if err := json.Unmarshal(data, &pairs); err != nil {
		return nil, fmt.Errorf("parse pairs %s: %w", d.PairsFile(), err)
	}
	return pairs, nil
}

// SavePairs atomically persists the sync-pair list.
func (d Dirs) SavePairs(pairs []SyncPair) error {
	data, err := json.MarshalIndent(pairs, "", "  ")
	if err != nil {
		return err
	}
	tmp := d.PairsFile() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write pairs: %w", err)
	}
	if err := os.Rename(tmp, d.PairsFile()); err != nil {
		return fmt.Errorf("commit pairs: %w", err)
	}
	return nil
}
