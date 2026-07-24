package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// The blacklist is the set of local files the user has chosen never to sync
// (typically because the server forbids the name and they don't want to rename).
// Entries are absolute local paths, compared case-insensitively to match
// Windows semantics.

// BlacklistFile is the path to the persisted blacklist.
func (d Dirs) BlacklistFile() string {
	return filepath.Join(d.Config, "blacklist.json")
}

// LoadBlacklist reads the set of blacklisted absolute local paths.
func (d Dirs) LoadBlacklist() (map[string]bool, error) {
	data, err := os.ReadFile(d.BlacklistFile())
	if errors.Is(err, os.ErrNotExist) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	var list []string
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(list))
	for _, p := range list {
		set[normalizePath(p)] = true
	}
	return set, nil
}

// AddBlacklist adds an absolute local path to the blacklist.
func (d Dirs) AddBlacklist(absPath string) error {
	set, err := d.LoadBlacklist()
	if err != nil {
		return err
	}
	set[normalizePath(absPath)] = true
	return d.saveBlacklist(set)
}

// RemoveBlacklist removes a path from the blacklist (so it can sync again).
func (d Dirs) RemoveBlacklist(absPath string) error {
	set, err := d.LoadBlacklist()
	if err != nil {
		return err
	}
	delete(set, normalizePath(absPath))
	return d.saveBlacklist(set)
}

func (d Dirs) saveBlacklist(set map[string]bool) error {
	list := make([]string, 0, len(set))
	for p := range set {
		list = append(list, p)
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := d.BlacklistFile() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, d.BlacklistFile())
}

// normalizePath canonicalises a path for set comparison (clean + lowercase).
func normalizePath(p string) string {
	return strings.ToLower(filepath.Clean(p))
}

// PathKey returns the canonical key used to look a path up in a blacklist set
// (as returned by LoadBlacklist).
func PathKey(p string) string { return normalizePath(p) }
