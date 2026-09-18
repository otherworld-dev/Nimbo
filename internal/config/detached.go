package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Detached is a folder that a share or mount left behind on this PC when it
// vanished from the account (Deck #557): someone stopped sharing it, the user
// was removed from its group, or the admin unmounted the storage. The copy is
// kept but PARKED — every sync pass leaves it alone — until the user says what
// to do with it (keep as their own, move it out, or delete it). See
// agent.ResolveDetached.
type Detached struct {
	LocalDir   string `json:"localDir"`   // the sync pair it lives in
	RemoteRoot string `json:"remoteRoot"` //
	Rel        string `json:"rel"`        // pair-relative, "/"-separated
	AtUnix     int64  `json:"at"`         // when it was parked
	// ParkedAt is where the copy was MOVED, when it could not stay in place:
	// an on-demand (cloud) folder can only show truthful state for things the
	// server has, so its salvaged copy is parked beside the sync folder. Empty
	// for a live-mode copy, which stays at LocalDir/Rel.
	ParkedAt string `json:"parkedAt,omitempty"`
}

// LocalPath is where the kept copy is right now.
func (x Detached) LocalPath() string {
	if x.ParkedAt != "" {
		return x.ParkedAt
	}
	return filepath.Join(x.LocalDir, filepath.FromSlash(x.Rel))
}

// DetachedFile is the per-account list of parked folders, scoped like PairsFile.
func (d Dirs) DetachedFile() string {
	if d.acct != "" {
		return filepath.Join(d.Config, "detached-"+d.acct+".json")
	}
	return filepath.Join(d.Config, "detached.json")
}

// LoadDetached returns the parked folders. A missing file is an empty list; an
// unreadable one is an error, because "nothing parked" would let a pass sync a
// copy the user has not decided about.
func (d Dirs) LoadDetached() ([]Detached, error) {
	data, err := os.ReadFile(d.DetachedFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read detached folders: %w", err)
	}
	var out []Detached
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse detached folders %s: %w", d.DetachedFile(), err)
	}
	return out, nil
}

// AddDetached parks a folder. Already parked — the same copy, at the same
// place, however the paths are spelled — is a no-op. Two copies of a folder
// unshared twice are distinct entries: they sit at different parked paths.
func (d Dirs) AddDetached(x Detached) error {
	cur, err := d.LoadDetached()
	if err != nil {
		return err
	}
	for _, c := range cur {
		if sameDetached(c, x) {
			return nil
		}
	}
	return d.saveDetached(append(cur, x))
}

// RemoveDetached un-parks the copy at localPath (see Detached.LocalPath). Not
// parked is not an error.
func (d Dirs) RemoveDetached(localPath string) error {
	cur, err := d.LoadDetached()
	if err != nil {
		return err
	}
	out := make([]Detached, 0, len(cur))
	for _, c := range cur {
		if PathKey(c.LocalPath()) != PathKey(localPath) {
			out = append(out, c)
		}
	}
	if len(out) == len(cur) {
		return nil
	}
	return d.saveDetached(out)
}

func sameDetached(a, b Detached) bool {
	return PathKey(a.LocalDir) == PathKey(b.LocalDir) && a.Rel == b.Rel &&
		PathKey(a.ParkedAt) == PathKey(b.ParkedAt)
}

func (d Dirs) saveDetached(list []Detached) error {
	if list == nil {
		list = []Detached{}
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(d.DetachedFile(), data, 0o600); err != nil {
		return fmt.Errorf("save detached folders: %w", err)
	}
	return nil
}
