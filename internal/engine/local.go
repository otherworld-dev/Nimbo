package engine

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// LocalScan walks the local sync root and returns a map of files-root-relative
// paths to their current state. The root itself is not included. OS/editor junk
// and in-progress temp files are skipped so they never drive sync decisions.
func LocalScan(root string) (map[string]LocalState, error) {
	return LocalScanScoped(root, "")
}

// LocalScanScoped is LocalScan limited to one subtree: it walks only root/scope
// (scope is a "/"-separated path relative to root; "" means the whole tree) but
// still keys results relative to root, so a scoped scan plugs straight into the
// same baseline/diff as a full one. If the scope directory no longer exists
// locally (its subtree was deleted), it returns an empty map so the diff
// propagates the deletions instead of erroring.
func LocalScanScoped(root, scope string) (map[string]LocalState, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("local root %q: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("local root %q is not a directory", root)
	}

	walkRoot := root
	if scope = strings.Trim(filepath.ToSlash(scope), "/"); scope != "" {
		walkRoot = filepath.Join(root, filepath.FromSlash(scope))
		if fi, serr := os.Stat(walkRoot); serr != nil || !fi.IsDir() {
			return make(map[string]LocalState), nil
		}
	}

	out := make(map[string]LocalState)
	err = filepath.WalkDir(walkRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == walkRoot {
			return nil
		}
		name := d.Name()
		if isIgnoredName(name) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)

		fi, err := d.Info()
		if err != nil {
			return err
		}
		st := LocalState{Path: rel, IsDir: d.IsDir(), MTime: fi.ModTime()}
		if !d.IsDir() {
			st.Size = fi.Size()
		}
		out[rel] = st
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk local root: %w", err)
	}
	return out, nil
}

// isIgnoredName reports whether a file/dir name should be excluded from sync.
// This is intentionally small for now; per-folder ignore rules arrive in Phase 6.
func isIgnoredName(name string) bool {
	switch name {
	case ".DS_Store", "Thumbs.db", "desktop.ini", ".Trash", ".Trashes":
		return true
	}
	// Note: server-forbidden names (.htaccess etc.) are NOT skipped here — they
	// are detected against the server's rules and surfaced as "can't sync" with
	// rename/blacklist options (see engine.Forbidden / FilterBlocked).
	// Editor/transfer temp files.
	if strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, "~") {
		return true
	}
	if strings.HasPrefix(name, "~$") || strings.HasPrefix(name, ".~") {
		return true
	}
	// Nimbo's own partial-download temp files (see transfer layer).
	if strings.HasSuffix(name, ".nimbo-part") {
		return true
	}
	return false
}
