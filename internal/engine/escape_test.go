package engine

import (
	"testing"
	"time"
)

// builtin forbidden includes .htaccess / .htpasswd / .user.ini; opt in by extension.
func newTestEscaper(exts ...string) *Escaper {
	return NewEscaper(NewForbidden(nil, nil, nil, nil, nil), exts, "")
}

func TestEscaperEncodeDecodeRoundTrip(t *testing.T) {
	e := newTestEscaper(".htaccess", ".htpasswd")

	cases := []struct {
		local   string
		server  string // expected Encode(local)
		escaped bool   // expected Decode(server).wasEscaped
	}{
		{".htaccess", ".htaccess.nimboesc", true},
		{"web/.htaccess", "web/.htaccess.nimboesc", true},
		{".htpasswd", ".htpasswd.nimboesc", true},
		{"secret.htpasswd", "secret.htpasswd", false}, // ext opted-in but the NAME isn't forbidden
		{"readme.txt", "readme.txt", false},           // not forbidden
		{".user.ini", ".user.ini", false},             // forbidden but ext .ini not opted in
		{"a/b/notes.md", "a/b/notes.md", false},       // ordinary file, nested
	}
	for _, c := range cases {
		if got := e.Encode(c.local); got != c.server {
			t.Errorf("Encode(%q) = %q, want %q", c.local, got, c.server)
		}
		dec, was := e.Decode(c.server)
		if was != c.escaped {
			t.Errorf("Decode(%q) wasEscaped = %v, want %v", c.server, was, c.escaped)
		}
		if was && dec != c.local {
			t.Errorf("Decode(%q) = %q, want round-trip back to %q", c.server, dec, c.local)
		}
	}
}

func TestEscaperIdempotent(t *testing.T) {
	e := newTestEscaper(".htaccess")
	once := e.Encode(".htaccess")
	if twice := e.Encode(once); twice != once {
		t.Errorf("Encode not idempotent: %q -> %q", once, twice)
	}
}

func TestEscaperDecodeLeavesGenuineFiles(t *testing.T) {
	e := newTestEscaper(".htaccess")
	// A real file that merely ends in the marker but doesn't decode to a forbidden
	// opted-in name must be left alone.
	for _, name := range []string{"photo.nimboesc", "report.htaccess.nimboesc", "data.nimboesc"} {
		if dec, was := e.Decode(name); was {
			t.Errorf("Decode(%q) wrongly un-escaped to %q", name, dec)
		}
	}
}

func TestEscaperInactiveIsNoop(t *testing.T) {
	e := newTestEscaper() // no extensions opted in
	if e.Active() {
		t.Fatal("escaper with no extensions should be inactive")
	}
	if got := e.Encode(".htaccess"); got != ".htaccess" {
		t.Errorf("inactive Encode mutated %q -> %q", ".htaccess", got)
	}
	if dec, was := e.Decode(".htaccess.nimboesc"); was || dec != ".htaccess.nimboesc" {
		t.Errorf("inactive Decode mutated: %q, %v", dec, was)
	}
	// A nil escaper must also be a safe no-op.
	var nilEsc *Escaper
	if nilEsc.Active() || nilEsc.Encode("x") != "x" {
		t.Error("nil escaper should be an inert no-op")
	}
}

// Regression: after an escaped upload, every reconcile pass — full scan, delta,
// and the watcher's per-path probe — must see the server copy under the LOCAL
// (decoded) key, or an existing escaped file is misread as "removed remotely"
// and deleted out from under a live server copy. And a genuine server-side
// delete of X<suffix> must still propagate as a local delete of X.
func TestEscapedReconcileNoChurnThenDeletePropagates(t *testing.T) {
	esc := newTestEscaper(".htaccess")
	const local = "Research/.htaccess"

	// The wiring contract: any remote probe for the file must target the
	// encoded server name (SyncPaths' stat, the executor's transfers).
	if got := esc.Encode(local); got != "Research/.htaccess.nimboesc" {
		t.Fatalf("Encode(%q) = %q, want the escaped server name", local, got)
	}

	// Baseline as recorded by the escaped upload: keyed by the LOCAL name.
	base := map[string]BaselineState{
		"Research": {Path: "Research", IsDir: true, RemoteETag: "d1"},
		local:      {Path: local, RemoteETag: "E", LocalSize: 3, LocalMTimeNanos: 111},
	}
	lc := map[string]LocalState{
		"Research": {Path: "Research", IsDir: true},
		local:      {Path: local, Size: 3, MTime: time.Unix(0, 111)},
	}

	// Steady state: the decoded scan (or escape-aware stat) reports the server
	// copy under the local key with the baseline etag → nothing to do.
	remote := map[string]RemoteState{
		"Research": {Path: "Research", IsDir: true, ETag: "d1"},
		local:      {Path: local, ETag: "E", Size: 3},
	}
	for _, a := range Diff(base, remote, lc) {
		if a.Kind != ActNoop {
			t.Errorf("steady state churn: %v %s (%s)", a.Kind, a.Path, a.Reason)
		}
	}

	// The pre-fix failure shape: a probe that misses the escaped server name
	// (remote map omits the file) must NOT be what callers feed Diff — it reads
	// as a remote deletion. This documents why the encode/decode hooks matter.
	blind := map[string]RemoteState{
		"Research": {Path: "Research", IsDir: true, ETag: "d1"},
	}
	sawDelete := false
	for _, a := range Diff(base, blind, lc) {
		if a.Path == local && a.Kind == ActDeleteLocal {
			sawDelete = true
		}
	}
	if !sawDelete {
		t.Error("expected a blind (un-escaped) probe to misread the file as removed remotely — the hazard the escape-aware probe prevents")
	}

	// Genuine server-side delete of X<suffix>: the decoded scan omits the file
	// and that MUST propagate as a local delete of X.
	gone := map[string]RemoteState{
		"Research": {Path: "Research", IsDir: true, ETag: "d2"},
	}
	got := ActNoop
	for _, a := range Diff(base, gone, lc) {
		if a.Path == local {
			got = a.Kind
		}
	}
	if got != ActDeleteLocal {
		t.Errorf("server delete propagation: got %v, want ActDeleteLocal", got)
	}
}

func TestEscaperBrandSuffix(t *testing.T) {
	e := NewEscaper(NewForbidden(nil, nil, nil, nil, nil), []string{".htaccess"}, ".acmeesc")
	if got := e.Encode(".htaccess"); got != ".htaccess.acmeesc" {
		t.Errorf("brand suffix: Encode(.htaccess) = %q, want .htaccess.acmeesc", got)
	}
	if dec, was := e.Decode(".htaccess.acmeesc"); !was || dec != ".htaccess" {
		t.Errorf("brand suffix: Decode = %q, %v", dec, was)
	}
}
