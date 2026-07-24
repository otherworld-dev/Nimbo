package engine

import (
	"path"
	"strings"
)

// DefaultEscapeSuffix is appended to a server-forbidden file's name so it can be
// stored on the server (e.g. ".htaccess" -> ".htaccess.nimboesc"). White-label
// builds may override it.
const DefaultEscapeSuffix = ".nimboesc"

// Escaper transparently renames server-forbidden files so they can still sync: a
// forbidden local name whose extension the user opted in is stored on the server
// with a marker suffix and decoded back on the way in. Encoding is confined to the
// transport edge — the diff, baseline and conflict logic only ever see LOCAL
// names. A nil or extension-less Escaper is a no-op, so callers never special-case
// "escaping off".
type Escaper struct {
	forbidden *Forbidden
	exts      map[string]bool // opted-in extensions (lowercased, leading dot)
	suffix    string
}

// NewEscaper builds an escaper for the forbidden matcher and the user's opted-in
// extensions. With no extensions it is inactive. An empty suffix defaults to
// DefaultEscapeSuffix.
func NewEscaper(f *Forbidden, exts []string, suffix string) *Escaper {
	if suffix == "" {
		suffix = DefaultEscapeSuffix
	}
	return &Escaper{forbidden: f, exts: lowerSet(normalizeExts(exts)), suffix: suffix}
}

// Active reports whether any escaping is configured.
func (e *Escaper) Active() bool { return e != nil && len(e.exts) > 0 }

// Suffix is the marker this escaper appends (for the disable migration / UI).
func (e *Escaper) Suffix() string {
	if e == nil || e.suffix == "" {
		return DefaultEscapeSuffix
	}
	return e.suffix
}

// optedIn reports whether a filename's extension is on the escape list.
func (e *Escaper) optedIn(base string) bool {
	return e.exts[strings.ToLower(path.Ext(base))]
}

// Escapes reports whether a filename is eligible to be escaped: its extension is
// opted-in AND the server actually forbids the name. FilterBlocked uses this to
// let escapable files through instead of blocking them.
func (e *Escaper) Escapes(base string) bool {
	if !e.Active() || !e.optedIn(base) {
		return false
	}
	_, blocked := e.forbidden.Check(base)
	return blocked
}

// WouldEscape reports whether opting in this basename's extension would let it
// sync — i.e. the name is forbidden now, but appending the marker suffix makes it
// allowed. False for names forbidden by a character (a suffix doesn't fix `a:b`),
// so the UI only offers escaping where it actually helps. Independent of the
// opted-in list, so it answers "would enabling this help?" for a blocked file.
func (e *Escaper) WouldEscape(base string) bool {
	if e == nil || e.forbidden == nil {
		return false
	}
	if _, bad := e.forbidden.Check(base); !bad {
		return false
	}
	_, stillBad := e.forbidden.Check(base + e.Suffix())
	return !stillBad
}

// Encode returns the server-side pair-relative path for a local one: a forbidden,
// opted-in basename gets the marker suffix appended. Idempotent (an already-escaped
// name isn't re-escaped). v1 escapes file names only; directory paths are untouched.
func (e *Escaper) Encode(rel string) string {
	if !e.Active() {
		return rel
	}
	base := path.Base(rel)
	if !e.Escapes(base) {
		return rel
	}
	return path.Join(path.Dir(rel), base+e.suffix)
}

// Decode reverses Encode for a server-side path: a basename ending in the marker
// whose decoded name we would have escaped is un-suffixed. Returns the decoded path
// and whether it changed. The "would have escaped" check (vs. a bare suffix strip)
// leaves a genuine file that merely ends in the marker untouched.
func (e *Escaper) Decode(rel string) (string, bool) {
	if !e.Active() {
		return rel, false
	}
	base := path.Base(rel)
	if !strings.HasSuffix(base, e.suffix) {
		return rel, false
	}
	decoded := strings.TrimSuffix(base, e.suffix)
	if decoded == "" || !e.Escapes(decoded) {
		return rel, false
	}
	return path.Join(path.Dir(rel), decoded), true
}
