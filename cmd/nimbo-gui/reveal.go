package main

import (
	"os"
	"path/filepath"
	"strings"
)

// activityLocalPath resolves an activity event's path to the absolute local
// path it names, or "" when the event has no owning folder.
//
// The feed records two shapes. Live-sync events are pair-relative, so
// remoteRoot must be "" for them — a live pair whose files happen to start
// with a folder named like its remote root must keep that segment. On-demand
// events carry the mount's remote root as a prefix (the watcher reports
// remoteFor(), which is files-root-relative), so the caller passes the mount's
// root and it is stripped. A rename is recorded as "old → new"; only the
// destination still exists, so that is the path resolved.
func activityLocalPath(localDir, remoteRoot, path string) string {
	if localDir == "" {
		return ""
	}
	if i := strings.LastIndex(path, " → "); i >= 0 {
		path = path[i+len(" → "):]
	}
	path = strings.Trim(path, "/")
	if root := strings.Trim(remoteRoot, "/"); root != "" {
		if path == root {
			path = ""
		} else if rest, ok := strings.CutPrefix(path, root+"/"); ok {
			path = rest
		}
	}
	if path == "" {
		return localDir
	}
	return filepath.Join(localDir, filepath.FromSlash(path))
}

// revealTarget picks what to show in the file manager for path: the item
// itself (select=true) when it still exists, otherwise the nearest ancestor
// folder that does (a deleted file opens the folder it was in). It never falls
// back to the volume root — if the sync folder itself has gone there is
// nothing useful to show, and the result is "".
//
// Lstat, not Stat: an online-only placeholder must count as present without
// being touched, and a dangling link is still something Explorer can select.
func revealTarget(path string) (target string, selectItem bool) {
	if path == "" {
		return "", false
	}
	if _, err := os.Lstat(path); err == nil {
		return path, true
	}
	for dir := filepath.Dir(path); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			return dir, false
		}
	}
	return "", false
}

// RevealPath shows a local file or folder in the file manager: the item
// selected in its folder when it exists, otherwise the nearest folder that
// still does. Returns false when there is nothing to show (the folder tree
// has gone), so the caller can fall back to something else.
func (a *App) RevealPath(path string) bool {
	target, sel := revealTarget(path)
	if target == "" {
		return false
	}
	if sel {
		revealPath(target)
	} else {
		openPath(target)
	}
	return true
}
