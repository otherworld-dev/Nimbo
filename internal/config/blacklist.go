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
	return d.updateBlacklist(func(set map[string]bool) { set[normalizePath(absPath)] = true })
}

// RemoveBlacklist removes a path from the blacklist (so it can sync again).
func (d Dirs) RemoveBlacklist(absPath string) error {
	return d.updateBlacklist(func(set map[string]bool) { delete(set, normalizePath(absPath)) })
}

// updateBlacklist reads, mutates and rewrites the list under the file's lock. A
// sync pass can walk into several forbidden names at once, and without the lock
// each addition would be overwritten by the stale set the next one had read.
func (d Dirs) updateBlacklist(mutate func(map[string]bool)) error {
	mu := fileMutex(d.BlacklistFile())
	mu.Lock()
	defer mu.Unlock()

	set, err := d.LoadBlacklist()
	if err != nil {
		return err
	}
	mutate(set)
	return d.saveBlacklist(set)
}

// saveBlacklist rewrites the file; callers hold the blacklist file's lock.
func (d Dirs) saveBlacklist(set map[string]bool) error {
	list := make([]string, 0, len(set))
	for p := range set {
		list = append(list, p)
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return writeFileLocked(d.BlacklistFile(), data, 0o600)
}

// normalizePath canonicalises a path for set comparison (clean + lowercase).
func normalizePath(p string) string {
	return strings.ToLower(filepath.Clean(p))
}

// PathKey returns the canonical key used to look a path up in a blacklist set
// (as returned by LoadBlacklist).
func PathKey(p string) string { return normalizePath(p) }
