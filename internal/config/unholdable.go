package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Unholdable is a server item this PC cannot sync because Windows cannot hold
// its name: one ending in a space or a dot (Deck #984). It is shown in "Can't
// sync" until somebody renames it, so the file is never just quietly missing.
// The list is kept on disk because a later pass may never list the folder it
// is in again (an unchanged folder's ETag lets a pass skip it).
type Unholdable struct {
	LocalDir   string `json:"localDir"`   // the sync pair (or on-demand folder) it belongs to
	RemoteRoot string `json:"remoteRoot"` // that folder's server root, files-root-relative
	Remote     string `json:"remote"`     // the item's server path, files-root-relative
	IsDir      bool   `json:"isDir,omitempty"`
}

// UnholdableFile is the per-account list, scoped like PairsFile.
func (d Dirs) UnholdableFile() string {
	if d.acct != "" {
		return filepath.Join(d.Config, "unholdable-"+d.acct+".json")
	}
	return filepath.Join(d.Config, "unholdable.json")
}

// LoadUnholdable returns the recorded items. A missing file is an empty list.
func (d Dirs) LoadUnholdable() ([]Unholdable, error) {
	data, err := os.ReadFile(d.UnholdableFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read unsyncable names: %w", err)
	}
	var out []Unholdable
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse unsyncable names %s: %w", d.UnholdableFile(), err)
	}
	return out, nil
}

// SaveUnholdable replaces the recorded items.
func (d Dirs) SaveUnholdable(list []Unholdable) error {
	if list == nil {
		list = []Unholdable{}
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(d.UnholdableFile(), data, 0o600); err != nil {
		return fmt.Errorf("save unsyncable names: %w", err)
	}
	return nil
}
