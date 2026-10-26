// Package winname holds the rules for server names Windows cannot represent
// faithfully. It is shared by on-demand mode (vfs), live sync (engine) and the
// GUI's placeholder listing so every path agrees on what is skipped.
package winname

import "strings"

// CannotHold reports whether a single path component has no faithful Windows
// counterpart: one ending in a space or a dot. The server allows those, but
// Win32 strips the trailing characters from every path component it is given,
// so a path through "Pilot Validation " silently reaches "Pilot Validation":
// another item, if one exists (Deck #984: a pulled file landed in the wrong
// folder, and reconcile then read the mismatch as the user's move and MOVEd it
// on the server). Such items are not synced at all.
func CannotHold(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	return strings.HasSuffix(name, " ") || strings.HasSuffix(name, ".")
}

// PathCannotHold reports whether any component of a slash-separated relative
// path is one CannotHold rejects.
func PathCannotHold(rel string) bool {
	for _, c := range strings.Split(rel, "/") {
		if CannotHold(c) {
			return true
		}
	}
	return false
}

// Equivalent reports whether two slash-separated paths name the same item once
// Win32 has trimmed each component (trailing spaces and dots) and ignored
// case: whether they are aliases, not different places.
func Equivalent(a, b string) bool {
	trim := func(p string) string {
		parts := strings.Split(strings.Trim(p, "/"), "/")
		for i, c := range parts {
			parts[i] = strings.TrimRight(c, " .")
		}
		return strings.Join(parts, "/")
	}
	return strings.EqualFold(trim(a), trim(b))
}
