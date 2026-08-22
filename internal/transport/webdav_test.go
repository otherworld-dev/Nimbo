package transport

import (
	"encoding/xml"
	"testing"
	"time"
)

func TestEscapePath(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", "/"},
		{"/", "/"},
		{"Documents", "/Documents"},
		{"/Documents/", "/Documents/"}, // trailing slash preserved by Split? see note
		{"Documents/report.pdf", "/Documents/report.pdf"},
		{"My Folder/a b.txt", "/My%20Folder/a%20b.txt"},
		{"weird/#hash?q.txt", "/weird/%23hash%3Fq.txt"},
		{"emoji/🚀.txt", "/emoji/%F0%9F%9A%80.txt"},
	}
	for _, tc := range tests {
		got := escapePath(tc.in)
		if got != tc.want {
			t.Errorf("escapePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestEscapeUnescapeRoundTrip ensures a path survives escaping into a URL and
// being parsed back out of an href, which is exactly the round trip a PROPFIND
// response goes through. This is the property that protects against path
// corruption for files with spaces and unicode.
func TestEscapeUnescapeRoundTrip(t *testing.T) {
	paths := []string{
		"Documents/report.pdf",
		"My Folder/a b.txt",
		"emoji/🚀.txt",
		"weird/#hash.txt",
		"deep/a/b/c/d.txt",
	}
	base := "/remote.php/dav/files/alice"
	for _, p := range paths {
		href := base + escapePath(p)
		decoded, err := unescapeHref(href)
		if err != nil {
			t.Fatalf("unescapeHref(%q): %v", href, err)
		}
		got := trimRel(decoded, base)
		if got != p {
			t.Errorf("round trip of %q = %q", p, got)
		}
	}
}

// trimRel mirrors how parseResponse recovers a files-root-relative path.
func trimRel(decoded, base string) string {
	rel := decoded
	if len(rel) >= len(base) && rel[:len(base)] == base {
		rel = rel[len(base):]
	}
	return trimSlashes(rel)
}

func trimSlashes(s string) string {
	for len(s) > 0 && s[0] == '/' {
		s = s[1:]
	}
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func TestParseResponseIsEncrypted(t *testing.T) {
	const body = `<?xml version="1.0"?>
<d:multistatus xmlns:d="DAV:" xmlns:nc="http://nextcloud.org/ns" xmlns:oc="http://owncloud.org/ns">
  <d:response>
    <d:href>/remote.php/dav/files/alice/Vault/</d:href>
    <d:propstat><d:status>HTTP/1.1 200 OK</d:status><d:prop>
      <d:resourcetype><d:collection/></d:resourcetype>
      <d:getetag>&quot;e1&quot;</d:getetag>
      <nc:is-encrypted>1</nc:is-encrypted>
    </d:prop></d:propstat>
  </d:response>
  <d:response>
    <d:href>/remote.php/dav/files/alice/Plain/</d:href>
    <d:propstat><d:status>HTTP/1.1 200 OK</d:status><d:prop>
      <d:resourcetype><d:collection/></d:resourcetype>
      <d:getetag>&quot;e2&quot;</d:getetag>
      <nc:is-encrypted>0</nc:is-encrypted>
    </d:prop></d:propstat>
  </d:response>
</d:multistatus>`
	var ms multistatus
	if err := xml.Unmarshal([]byte(body), &ms); err != nil {
		t.Fatal(err)
	}
	c := New("https://cloud.example.com", "alice", "pw")
	byPath := map[string]Entry{}
	for _, r := range ms.Responses {
		if e, ok := c.parseResponse("/remote.php/dav/files/alice", r); ok {
			byPath[e.Path] = e
		}
	}
	v, ok := byPath["Vault"]
	if !ok || !v.IsEncrypted {
		t.Errorf("Vault: ok=%v IsEncrypted=%v, want encrypted dir", ok, v.IsEncrypted)
	}
	p, ok := byPath["Plain"]
	if !ok || p.IsEncrypted {
		t.Errorf("Plain: ok=%v IsEncrypted=%v, want plain dir", ok, p.IsEncrypted)
	}
}

// TestParseResponseLock covers the files_lock properties. The fixture mirrors
// what a live Nextcloud 34.0.2 actually returns (see
// docs/plans/2026-08-08-file-locking-findings.md): an UNLOCKED file reports
// nc:lock as an empty string in the 200 block and puts every other lock property
// in a 404 propstat, and nc:lock-timeout can be negative.
func TestParseResponseLock(t *testing.T) {
	const body = `<?xml version="1.0"?>
<d:multistatus xmlns:d="DAV:" xmlns:nc="http://nextcloud.org/ns" xmlns:oc="http://owncloud.org/ns">
  <d:response>
    <d:href>/remote.php/dav/files/alice/Team/Budget.xlsx</d:href>
    <d:propstat><d:status>HTTP/1.1 200 OK</d:status><d:prop>
      <d:getetag>&quot;e1&quot;</d:getetag>
      <nc:lock>1</nc:lock>
      <nc:lock-owner>bob</nc:lock-owner>
      <nc:lock-owner-displayname>Bob Smith</nc:lock-owner-displayname>
      <nc:lock-owner-type>1</nc:lock-owner-type>
      <nc:lock-time>1786228737</nc:lock-time>
      <nc:lock-timeout>1800</nc:lock-timeout>
      <nc:lock-token>files_lock/c1339370-96d3-4151-a2a3-5f193a1cd67b</nc:lock-token>
    </d:prop></d:propstat>
  </d:response>
  <d:response>
    <d:href>/remote.php/dav/files/alice/Team/Free.xlsx</d:href>
    <d:propstat><d:status>HTTP/1.1 200 OK</d:status><d:prop>
      <d:getetag>&quot;e2&quot;</d:getetag>
      <nc:lock></nc:lock>
    </d:prop></d:propstat>
    <d:propstat><d:status>HTTP/1.1 404 Not Found</d:status><d:prop>
      <nc:lock-owner/><nc:lock-owner-displayname/><nc:lock-owner-type/>
      <nc:lock-time/><nc:lock-timeout/><nc:lock-token/>
    </d:prop></d:propstat>
  </d:response>
  <d:response>
    <d:href>/remote.php/dav/files/alice/Team/NoApp.xlsx</d:href>
    <d:propstat><d:status>HTTP/1.1 200 OK</d:status><d:prop>
      <d:getetag>&quot;e3&quot;</d:getetag>
    </d:prop></d:propstat>
  </d:response>
  <d:response>
    <d:href>/remote.php/dav/files/alice/Team/NoExpiry.xlsx</d:href>
    <d:propstat><d:status>HTTP/1.1 200 OK</d:status><d:prop>
      <d:getetag>&quot;e4&quot;</d:getetag>
      <nc:lock>1</nc:lock>
      <nc:lock-owner>adam</nc:lock-owner>
      <nc:lock-owner-type>0</nc:lock-owner-type>
      <nc:lock-timeout>-60</nc:lock-timeout>
    </d:prop></d:propstat>
  </d:response>
</d:multistatus>`
	var ms multistatus
	if err := xml.Unmarshal([]byte(body), &ms); err != nil {
		t.Fatal(err)
	}
	c := New("https://cloud.example.com", "alice", "pw")
	byPath := map[string]Entry{}
	for _, r := range ms.Responses {
		if e, ok := c.parseResponse("/remote.php/dav/files/alice", r); ok {
			byPath[e.Path] = e
		}
	}

	locked := byPath["Team/Budget.xlsx"]
	if locked.Lock == nil {
		t.Fatal("Budget.xlsx: Lock is nil, want a lock record")
	}
	if locked.Lock.Owner != "bob" || locked.Lock.OwnerDisplay != "Bob Smith" {
		t.Errorf("owner = %q/%q, want bob/Bob Smith", locked.Lock.Owner, locked.Lock.OwnerDisplay)
	}
	if locked.Lock.OwnerType != LockOwnerApp {
		t.Errorf("OwnerType = %v, want LockOwnerApp", locked.Lock.OwnerType)
	}
	if locked.Lock.Token != "files_lock/c1339370-96d3-4151-a2a3-5f193a1cd67b" {
		t.Errorf("Token = %q", locked.Lock.Token)
	}
	if locked.Lock.Timeout != 1800*time.Second {
		t.Errorf("Timeout = %v, want 30m", locked.Lock.Timeout)
	}
	if !locked.Lock.Since.Equal(time.Unix(1786228737, 0)) {
		t.Errorf("Since = %v, want the epoch-seconds value", locked.Lock.Since)
	}

	// Explicitly unlocked, and "the server never answered", must BOTH be nil: the
	// feature fails open and never claims a file is locked on thin evidence.
	if e := byPath["Team/Free.xlsx"]; e.Lock != nil {
		t.Errorf("Free.xlsx: Lock = %+v, want nil (empty nc:lock means unlocked)", e.Lock)
	}
	if e := byPath["Team/NoApp.xlsx"]; e.Lock != nil {
		t.Errorf("NoApp.xlsx: Lock = %+v, want nil (no files_lock app)", e.Lock)
	}

	// A negative nc:lock-timeout is not "seconds remaining" — it must leave
	// Timeout zero, meaning "the server set no expiry".
	noExp := byPath["Team/NoExpiry.xlsx"]
	if noExp.Lock == nil {
		t.Fatal("NoExpiry.xlsx: Lock is nil, want a lock record")
	}
	if noExp.Lock.Timeout != 0 {
		t.Errorf("Timeout = %v, want 0 for a negative nc:lock-timeout", noExp.Lock.Timeout)
	}
	if noExp.Lock.OwnerType != LockOwnerUser {
		t.Errorf("OwnerType = %v, want LockOwnerUser", noExp.Lock.OwnerType)
	}
}

// A LOCK reply is a bare <d:prop> document, not a multistatus — a different
// shape from every other response this package parses. It carries the token AND
// the file's new ETag (locking bumps it), so one call tells the client
// everything and no follow-up PROPFIND is needed.
func TestParseLockResult(t *testing.T) {
	const body = `<?xml version="1.0"?>
<d:prop xmlns:d="DAV:" xmlns:s="http://sabredav.org/ns" xmlns:oc="http://owncloud.org/ns" xmlns:nc="http://nextcloud.org/ns">
  <d:getetag>696b29b8bbd95787354b2d8bded5e0a8</d:getetag>
  <nc:lock>1</nc:lock>
  <nc:lock-owner>adam</nc:lock-owner>
  <nc:lock-owner-displayname>Adam</nc:lock-owner-displayname>
  <nc:lock-owner-type>0</nc:lock-owner-type>
  <nc:lock-timeout>0</nc:lock-timeout>
  <nc:lock-token>files_lock/0c078830-7dae-4e8c-8293-994c8a9d080c</nc:lock-token>
</d:prop>`
	got, err := parseLockResult([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != "files_lock/0c078830-7dae-4e8c-8293-994c8a9d080c" {
		t.Errorf("Token = %q", got.Token)
	}
	if got.ETag != "696b29b8bbd95787354b2d8bded5e0a8" {
		t.Errorf("ETag = %q, want the new etag from the LOCK body", got.ETag)
	}
	if got.Info.Owner != "adam" || got.Info.OwnerType != LockOwnerUser {
		t.Errorf("Info = %+v", got.Info)
	}
	if got.Info.Token != got.Token {
		t.Errorf("Info.Token = %q, want it to match", got.Info.Token)
	}

	// An app lock's reply names the app and no person.
	const appBody = `<?xml version="1.0"?>
<d:prop xmlns:d="DAV:" xmlns:nc="http://nextcloud.org/ns">
  <nc:lock>1</nc:lock>
  <nc:lock-owner-displayname>Text</nc:lock-owner-displayname>
  <nc:lock-owner-editor>text</nc:lock-owner-editor>
  <nc:lock-owner-type>1</nc:lock-owner-type>
  <nc:lock-token>files_lock/abc</nc:lock-token>
</d:prop>`
	app, err := parseLockResult([]byte(appBody))
	if err != nil {
		t.Fatal(err)
	}
	if app.Info.AppName() != "Text" || app.Info.Owner != "" {
		t.Errorf("app lock: AppName=%q Owner=%q, want Text and no person", app.Info.AppName(), app.Info.Owner)
	}
}

func TestLockedByOther(t *testing.T) {
	cases := []struct {
		name  string
		lock  *LockInfo
		login string
		want  bool
	}{
		{"no lock at all", nil, "alice", false},
		{"locked by someone else", &LockInfo{Owner: "bob"}, "alice", true},
		{"locked by me", &LockInfo{Owner: "alice"}, "alice", false},
		{"locked by me, different case", &LockInfo{Owner: "Alice"}, "alice", false},
		{"lock we cannot attribute", &LockInfo{Owner: ""}, "alice", true},
		{"we do not know who we are", &LockInfo{Owner: "bob"}, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := (Entry{Lock: c.lock}).LockedByOther(c.login); got != c.want {
				t.Errorf("LockedByOther(%q) = %v, want %v", c.login, got, c.want)
			}
		})
	}
}

func TestServerReadOnly(t *testing.T) {
	cases := []struct {
		perm string
		dir  bool
		want bool
	}{
		{"MG", true, true},       // .Collectives root: mounted+read, no create -> read-only
		{"RMGCK", true, false},   // a collective: can create file/folder -> writable
		{"RGDNVW", false, false}, // normal file: has W -> writable
		{"RG", false, true},      // read-only shared file: no W -> read-only
		{"", true, false},        // unknown -> treat as writable (never wrongly lock)
		{"", false, false},
	}
	for _, c := range cases {
		e := Entry{Permissions: c.perm, IsDir: c.dir}
		if got := e.ServerReadOnly(); got != c.want {
			t.Errorf("ServerReadOnly(perm=%q dir=%v) = %v, want %v", c.perm, c.dir, got, c.want)
		}
	}
}

func TestEntryContentSHA1(t *testing.T) {
	cases := []struct{ in, want string }{
		{"SHA1:ABC123 MD5:dead", "abc123"},
		{"MD5:dead SHA1:cafe", "cafe"},
		{"sha1:Feed", "feed"},
		{"MD5:dead ADLER32:0001", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := (Entry{Checksums: c.in}).ContentSHA1(); got != c.want {
			t.Errorf("ContentSHA1(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
