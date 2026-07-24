package engine

import (
	"path"
	"strings"
)

// builtinForbidden are names every Nextcloud rejects; used as a fallback in case
// the server's capabilities omit them (older servers).
var builtinForbidden = []string{".htaccess", ".htpasswd", ".user.ini"}

// Forbidden tests filenames against the server's rules (from the files
// capability) plus a small builtin fallback, so the client can flag files the
// server would refuse rather than uploading them and failing repeatedly.
type Forbidden struct {
	names     map[string]bool // exact full filenames (lowercased)
	basenames map[string]bool // names without extension (lowercased)
	exts      map[string]bool // extensions incl. leading dot (lowercased)
	chars     []string        // forbidden characters/substrings
	allow     map[string]bool // exact filenames the user force-allows (lowercased)
}

// NewForbidden builds a matcher from the server-provided lists (any may be nil).
// allow lists exact filenames the user has chosen to permit even if otherwise
// blocked (e.g. ".htaccess" when their server accepts it).
func NewForbidden(names, basenames, chars, exts, allow []string) *Forbidden {
	f := &Forbidden{
		names:     lowerSet(append(append([]string{}, names...), builtinForbidden...)),
		basenames: lowerSet(basenames),
		exts:      lowerSet(normalizeExts(exts)),
		allow:     lowerSet(allow),
	}
	f.chars = chars
	return f
}

// Check reports whether a filename is forbidden and, if so, a short reason.
func (f *Forbidden) Check(name string) (reason string, blocked bool) {
	if f == nil || name == "" {
		return "", false
	}
	lower := strings.ToLower(name)
	if f.allow[lower] {
		return "", false // user explicitly allowed this exact filename
	}
	if f.names[lower] {
		return "filename not allowed by the server", true
	}
	ext := strings.ToLower(path.Ext(lower))
	if ext != "" && f.exts[ext] {
		return "file extension " + ext + " is not allowed by the server", true
	}
	base := lower
	if ext != "" {
		base = strings.TrimSuffix(lower, ext)
	}
	if f.basenames[base] {
		return "filename not allowed by the server", true
	}
	for _, ch := range f.chars {
		if ch != "" && strings.Contains(name, ch) {
			return "filename contains a character the server rejects (" + ch + ")", true
		}
	}
	return "", false
}

// Blocked is a path that cannot be synced because its name is forbidden.
type Blocked struct {
	Path   string // pair-relative path
	Reason string
	IsDir  bool
}

// FilterBlocked splits a plan into actions that can proceed and uploads/creates
// whose names the server forbids. Paths for which blacklisted returns true are
// dropped silently (the user chose to ignore them); forbidden ones are returned
// as Blocked so a UI can offer rename/blacklist. Non-upload actions pass through.
func FilterBlocked(actions []Action, f *Forbidden, esc *Escaper, blacklisted func(rel string) bool) (kept []Action, blocked []Blocked) {
	// Count how many actions land on each server-side name, so an escaped forbidden
	// X (stored as X.nimboesc) that a real X.nimboesc also claims blocks X rather
	// than overwriting it. Encode is a no-op when escaping is inactive.
	claimed := make(map[string]int)
	for _, a := range actions {
		switch a.Kind {
		case ActUpload, ActCreateRemoteDir:
			claimed[esc.Encode(a.Path)]++
		case ActMoveRemote:
			claimed[esc.Encode(a.Dest)]++
		}
	}
	for _, a := range actions {
		target := a.Path
		isDir := a.Kind == ActCreateRemoteDir
		switch a.Kind {
		case ActUpload, ActCreateRemoteDir:
			// target = a.Path
		case ActMoveRemote:
			target = a.Dest // the new remote name is what must be valid
		default:
			kept = append(kept, a)
			continue
		}

		if blacklisted != nil && blacklisted(target) {
			continue // user-ignored: drop without flagging
		}
		if reason, bad := f.Check(path.Base(target)); bad {
			// A forbidden name whose extension the user opted in is escaped by the
			// executor (stored under a marker name) instead of blocked — unless that
			// would collide with a real file already occupying the server name.
			if esc.Escapes(path.Base(target)) && claimed[esc.Encode(target)] == 1 {
				kept = append(kept, a)
				continue
			}
			blocked = append(blocked, Blocked{Path: target, Reason: reason, IsDir: isDir})
			continue
		}
		kept = append(kept, a)
	}
	return kept, blocked
}

func lowerSet(in []string) map[string]bool {
	m := make(map[string]bool, len(in))
	for _, s := range in {
		s = strings.ToLower(strings.TrimSpace(s))
		if s != "" {
			m[s] = true
		}
	}
	return m
}

// normalizeExts ensures each extension has a single leading dot.
func normalizeExts(in []string) []string {
	out := make([]string, 0, len(in))
	for _, e := range in {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		out = append(out, e)
	}
	return out
}
