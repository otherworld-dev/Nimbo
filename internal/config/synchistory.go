package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// SyncHistoryFile is the path to the per-account record of pair keys that have
// completed an initial clone, scoped like PairsFile. It lives in Config, NOT
// Data, deliberately: its whole job is detecting that the Data-side sync
// database vanished (state reset), so it must not share that fate.
func (d Dirs) SyncHistoryFile() string {
	if d.acct != "" {
		return filepath.Join(d.Config, "synced-"+d.acct+".json")
	}
	return filepath.Join(d.Config, "synced.json")
}

// LoadSyncHistory reads the set of pair keys that have ever completed an
// initial clone. A missing file yields an empty, non-nil set.
func (d Dirs) LoadSyncHistory() (map[string]bool, error) {
	data, err := os.ReadFile(d.SyncHistoryFile())
	if errors.Is(err, os.ErrNotExist) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read sync history: %w", err)
	}
	var keys []string
	if err := json.Unmarshal(data, &keys); err != nil {
		return nil, fmt.Errorf("parse sync history %s: %w", d.SyncHistoryFile(), err)
	}
	out := make(map[string]bool, len(keys))
	for _, k := range keys {
		out[k] = true
	}
	return out, nil
}

// MarkPairSynced records that a pair completed its initial clone. Idempotent;
// atomically rewrites the file (tmp + rename, like SavePairs).
func (d Dirs) MarkPairSynced(pairKey string) error {
	hist, err := d.LoadSyncHistory()
	if err != nil {
		// Unreadable/corrupt history: rebuild rather than wedge the tripwire
		// forever. Lost entries re-seed themselves — the engine backfills the
		// marker for every already-synced pair each run.
		hist = map[string]bool{}
	}
	if hist[pairKey] {
		return nil
	}
	hist[pairKey] = true
	keys := make([]string, 0, len(hist))
	for k := range hist {
		keys = append(keys, k)
	}
	data, err := json.MarshalIndent(keys, "", "  ")
	if err != nil {
		return err
	}
	tmp := d.SyncHistoryFile() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write sync history: %w", err)
	}
	if err := os.Rename(tmp, d.SyncHistoryFile()); err != nil {
		return fmt.Errorf("commit sync history: %w", err)
	}
	return nil
}
