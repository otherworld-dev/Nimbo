package engine

import (
	"path"
	"strings"
)

// defaultIgnore are patterns excluded out of the box: editor lock/temp files, OS
// cruft, and other sync clients' internal journals — never user data (syncing
// them would just pollute the server, and they collide when taking over an
// existing folder). Dependency/VCS dirs (node_modules, .git, …) are NOT hard-
// coded here: the agent seeds them into the user-editable global ignore list
// instead (see agent.seedDevIgnores), so they're excluded by default — cold-
// crawling them, one PROPFIND per directory, can drive a small Nextcloud host
// into the ground — yet visible in Settings → Exclusions and removable by users
// who accept the cost. (.git also deserves caution on its own merits: file-
// syncing a live repo across machines can corrupt it.)
var defaultIgnore = []string{
	"*~", "~$*", ".~lock.*", "*.tmp", "*.part",
	".DS_Store", "Thumbs.db", "desktop.ini",
	// official Nextcloud/ownCloud desktop client state (e.g. after a takeover):
	".sync_*.db", ".sync_*.db-shm", ".sync_*.db-wal", "._sync_*.db",
	".owncloudsync.log*", ".nextcloudsync.log*", "*.~syncpart",
}

// Ignore matches paths against gitignore-lite glob patterns so they are excluded
// from sync entirely (neither uploaded nor downloaded, and not deleted):
//
//   - a pattern with no "/" matches a path segment's basename at any depth
//     (e.g. "*.log", "node_modules");
//   - a pattern containing "/" matches the full pair-relative path
//     (e.g. "build/out");
//   - a trailing "/" (a directory marker) is accepted and ignored — subtree
//     exclusion is handled by also testing every ancestor segment.
type Ignore struct {
	patterns []string
}

// NewIgnore builds a matcher from the given patterns plus the built-in defaults.
func NewIgnore(patterns []string) *Ignore {
	all := append(append([]string{}, defaultIgnore...), patterns...)
	cleaned := all[:0]
	for _, p := range all {
		if p = strings.TrimSpace(p); p != "" && !strings.HasPrefix(p, "#") {
			cleaned = append(cleaned, p)
		}
	}
	return &Ignore{patterns: cleaned}
}

// Match reports whether a pair-relative path is ignored. A path is ignored if it
// or any of its ancestor directories matches a pattern (so excluding a directory
// excludes everything under it).
func (ig *Ignore) Match(rel string) bool {
	if ig == nil || rel == "" {
		return false
	}
	parts := strings.Split(rel, "/")
	for i := 1; i <= len(parts); i++ {
		ancestor := strings.Join(parts[:i], "/")
		base := parts[i-1]
		for _, p := range ig.patterns {
			if matchPattern(p, ancestor, base) {
				return true
			}
		}
	}
	return false
}

func matchPattern(pattern, fullPath, base string) bool {
	pattern = strings.TrimSuffix(pattern, "/")
	if pattern == "" {
		return false
	}
	if strings.Contains(pattern, "/") {
		ok, _ := path.Match(pattern, fullPath)
		return ok
	}
	ok, _ := path.Match(pattern, base)
	return ok
}

// FilterLocal removes ignored entries from a local-state map in place.
func (ig *Ignore) FilterLocal(m map[string]LocalState) {
	for k := range m {
		if ig.Match(k) {
			delete(m, k)
		}
	}
}

// FilterRemote removes ignored entries from a remote-state map in place.
func (ig *Ignore) FilterRemote(m map[string]RemoteState) {
	for k := range m {
		if ig.Match(k) {
			delete(m, k)
		}
	}
}
