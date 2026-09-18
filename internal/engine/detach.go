package engine

import (
	"sort"
	"strings"
)

// KeepDetached rewrites a plan so that a share or mount which has been DETACHED
// from the account — a folder someone stopped sharing with you, a group folder
// you were removed from, an external storage the admin unmounted — is left
// alone locally instead of deleted.
//
// Such a folder simply vanishes from the WebDAV tree, which the diff cannot
// tell apart from its owner deleting the contents: both are "in the baseline,
// gone from the server, still here" and both plan a local delete of the whole
// subtree. Below the damage guard's thresholds that delete used to be permanent
// (Deck #557); it now goes to the Recycle Bin, but an unshare also puts nothing
// in the server-side trash, so the local copy is the only one the user has.
//
// What tells the two apart is the baseline: a vanished path whose row is a
// MountRoot was the top of a share or mount, and nothing above it changed.
// For every such root the plan loses every action at or beneath it — the
// deletes, but also any conflict or upload for a file under it, since acting on
// those would recreate the share's content on the server. The caller drops the
// subtree's baseline rows and tells the user; the copy on disk is untouched.
//
// A mount root whose own ancestor is being deleted in the same plan is NOT
// detached: the user deleted the parent folder (elsewhere), taking the mount
// with it, and keeping the share would leave the parent behind and re-upload it
// next pass. Only a local delete counts — a root the USER removed locally is a
// server-side delete to propagate, exactly as before.
func KeepDetached(actions []Action, base map[string]BaselineState) (out []Action, detached []string) {
	deleting := make(map[string]bool)
	for _, a := range actions {
		if a.Kind == ActDeleteLocal {
			deleting[a.Path] = true
		}
	}
	for p := range deleting {
		if b, ok := base[p]; !ok || !b.MountRoot || ancestorDeleted(p, deleting) {
			continue
		}
		detached = append(detached, p)
	}
	if len(detached) == 0 {
		return actions, nil
	}
	sort.Strings(detached)
	out = make([]Action, 0, len(actions))
	for _, a := range actions {
		if underAny(a.Path, detached) {
			continue
		}
		out = append(out, a)
	}
	return out, detached
}

// ancestorDeleted reports whether any directory above p is itself planned for
// local deletion.
func ancestorDeleted(p string, deleting map[string]bool) bool {
	for d := parentOf(p); d != ""; d = parentOf(d) {
		if deleting[d] {
			return true
		}
	}
	return false
}

// underAny reports whether p is one of roots or lies beneath one, matching
// whole path segments ("Teams" is not under "Team").
func underAny(p string, roots []string) bool {
	for _, r := range roots {
		if p == r || strings.HasPrefix(p, r+"/") {
			return true
		}
	}
	return false
}
