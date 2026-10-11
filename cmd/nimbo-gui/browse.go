package main

import (
	"path"
	"sort"
	"strings"

	"github.com/otherworld/nimbo/internal/config"
	"github.com/otherworld/nimbo/internal/transport"
)

// browseEntries turns one PROPFIND level of dir into the folder picker's rows:
// the listed dir itself dropped, anything the account's ignore rules exclude
// dropped (so the picker never offers .git, node_modules, desktop.ini, the
// other client's journals and so on), folders first, then A-Z ignoring case.
//
// ignored is asked about the path relative to the sync pair the entry sits
// in, because that is how the engine applies the rules: a pattern with a "/"
// in it is matched against the pair-relative path. pairRel does that mapping.
func browseEntries(dir string, entries []transport.Entry, pairRel func(string) string, ignored func(string) bool) []BrowseEntry {
	trimmed := strings.Trim(dir, "/")
	out := make([]BrowseEntry, 0, len(entries))
	for _, e := range entries {
		p := strings.Trim(e.Path, "/")
		if p == trimmed {
			continue
		}
		if ignored != nil && ignored(pairRel(p)) {
			continue
		}
		out = append(out, BrowseEntry{Name: path.Base(p), Path: p, IsDir: e.IsDir, Size: e.Size})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		a, b := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if a != b {
			return a < b
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// pairRelFunc maps an account path to the path the ignore rules see: relative
// to the most specific sync pair whose remote root holds it, or the account
// path itself when no pair does (the add-a-folder flow browsing new ground).
func pairRelFunc(pairs []config.SyncPair) func(string) string {
	roots := make([]string, 0, len(pairs))
	for _, p := range pairs {
		roots = append(roots, strings.Trim(p.RemoteRoot, "/"))
	}
	return func(p string) string {
		best, found := "", false
		for _, r := range roots {
			if r == "" || p == r || strings.HasPrefix(p, r+"/") {
				if !found || len(r) > len(best) {
					best, found = r, true
				}
			}
		}
		if !found || best == "" {
			return p
		}
		return strings.TrimPrefix(strings.TrimPrefix(p, best), "/")
	}
}
