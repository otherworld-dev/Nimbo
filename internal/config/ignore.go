package config

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// The global ignore list applies to every sync pair (in addition to each pair's
// own Excludes and the engine's built-in defaults). It is stored as one glob
// pattern per line.

// IgnoreFile is the path to the global ignore patterns file.
func (d Dirs) IgnoreFile() string {
	return filepath.Join(d.Config, "ignore.conf")
}

// LoadIgnore reads the global ignore patterns. A missing file yields nil.
func (d Dirs) LoadIgnore() ([]string, error) {
	f, err := os.Open(d.IgnoreFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var patterns []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			patterns = append(patterns, line)
		}
	}
	return patterns, sc.Err()
}

// SaveIgnore writes the global ignore patterns (one per line).
func (d Dirs) SaveIgnore(patterns []string) error {
	tmp := d.IgnoreFile() + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(patterns, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, d.IgnoreFile())
}
