package transport

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
		e, ok, err := c.parseResponse(r)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
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
// the 2026-08-08 file-locking findings): an UNLOCKED file reports
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
      <oc:owner-id>alice</oc:owner-id>
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
		e, ok, err := c.parseResponse(r)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
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
	// Who owns the FILE, not the lock: the owner may clear a stale lock (#733).
	if locked.Lock.FileOwner != "alice" {
		t.Errorf("FileOwner = %q, want alice (oc:owner-id)", locked.Lock.FileOwner)
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

// OnMount reads the two oc:permissions letters that say "this node lives on a
// storage that can be detached from the account": S (a share received from
// someone else) and M (an external storage or group folder mount). The letters
// were sampled from a live Nextcloud; a home-storage node carries neither.
func TestEntryOnMount(t *testing.T) {
	cases := []struct {
		perm string
		want bool
	}{
		{"SRGDNVCK", true}, // a folder shared with me (root or any descendant)
		{"SRGDNVW", true},  // a file inside a received share
		{"MG", true},       // .Collectives root: a mount
		{"RMGCK", true},    // a collective inside the mount
		{"RGDNVCK", false}, // my own folder
		{"RGDNVW", false},  // my own file
		{"", false},        // unknown: never guess "detachable"
	}
	for _, c := range cases {
		if got := (Entry{Permissions: c.perm}).OnMount(); got != c.want {
			t.Errorf("OnMount(%q) = %v, want %v", c.perm, got, c.want)
		}
	}
}

// GetRange is what hydration uses instead of GetFrom's open-ended "download
// from here to EOF": one GET for exactly the caller's span, so a large file
// no longer costs one HTTP request per megabyte.
func TestGetRangeSendsABoundedRange(t *testing.T) {
	var gotRange string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.Header.Get("Range")
		w.Header().Set("Content-Range", "bytes 4096-8191/16384")
		w.WriteHeader(http.StatusPartialContent)
		w.Write(bytes.Repeat([]byte{7}, 4096))
	}))
	defer srv.Close()
	c := New(srv.URL, "adam", "app-password")
	body, _, err := c.GetRange(context.Background(), "f.bin", 4096, 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	data, _ := io.ReadAll(body)
	if gotRange != "bytes=4096-8191" {
		t.Errorf("Range = %q, want bytes=4096-8191", gotRange)
	}
	if len(data) != 4096 {
		t.Errorf("read %d bytes, want 4096", len(data))
	}
}

// A 200 to a ranged request means the server ignored the Range header and is
// about to hand back the WHOLE file starting at byte 0 — silently accepting
// that for a non-zero offset would splice unrelated bytes into the caller's
// buffer as if they were the requested range.
func TestGetRangeRejectsAFullBodyForANonZeroOffset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // the server ignored the Range header
		w.Write(make([]byte, 100))
	}))
	defer srv.Close()
	c := New(srv.URL, "adam", "app-password")
	if _, _, err := c.GetRange(context.Background(), "f.bin", 50, 10); !errors.Is(err, ErrRangeIgnored) {
		t.Fatalf("err = %v, want ErrRangeIgnored", err)
	}
}

// A 200 at offset 0 is fine either way: reading from the start is still the
// right bytes, whether or not the server bothered with 206. (Trimming the
// body to exactly the requested length is the engine's OpenRange, layered on
// top; GetRange itself just hands back whatever the server sent.)
func TestGetRangeAcceptsAFullBodyAtOffsetZero(t *testing.T) {
	want := bytes.Repeat([]byte{9}, 20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(want)
	}))
	defer srv.Close()
	c := New(srv.URL, "adam", "app-password")
	body, _, err := c.GetRange(context.Background(), "f.bin", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	data, _ := io.ReadAll(body)
	if !bytes.Equal(data, want) {
		t.Errorf("read %q, want %q", data, want)
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

// A listing of a path the server does not have is final, not transient. The
// on-demand delete guard lists a vanished path before deleting it; reading a
// 404 as a network failure retried every such delete forever, and the error
// text must still say "not found" for Stat, which matches on it.
func TestPropFindNotFoundIsFinal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := New(srv.URL, "adam", "app-password")
	_, err := c.PropFind(context.Background(), "gone", 1)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("PROPFIND 404: err = %v, want ErrNotFound", err)
	}
	if Retryable(err) {
		t.Error("a PROPFIND 404 is classed as worth retrying")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error text %q lost the \"not found\" Stat matches on", err)
	}
	_, ok, serr := c.Stat(context.Background(), "gone")
	if serr != nil || ok {
		t.Errorf("Stat of a missing path = %v, %v; want false, nil", ok, serr)
	}
}

// Stat's "absent" must mean the server said 404 and nothing else. It matched
// the words "not found" in the error text, and the text carries the path, so
// a refusal of a file NAMED like that read as "not on the server" — which a
// sync pass acts on by deleting the local copy (Deck #691).
func TestStatReportsAbsentOnlyForA404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "gone") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	c := New(srv.URL, "adam", "app-password")

	if _, found, err := c.Stat(context.Background(), "gone.txt"); err != nil || found {
		t.Fatalf("404: found=%v err=%v, want absent with no error", found, err)
	}
	if _, found, err := c.Stat(context.Background(), "Items not found.xlsx"); err == nil {
		t.Fatalf("403 on a file named 'not found': found=%v, want an error, not absent", found)
	}
}

// TestParseResponseUploadTime covers nc:upload_time: the one property a live
// Nextcloud 34 changes on every content upload but leaves alone when files_lock
// takes or releases a lock (measured 2026-09-23 — the lock bumps the ETag and
// nothing else).
func TestParseResponseUploadTime(t *testing.T) {
	const body = `<?xml version="1.0"?>
<d:multistatus xmlns:d="DAV:" xmlns:nc="http://nextcloud.org/ns" xmlns:oc="http://owncloud.org/ns">
  <d:response>
    <d:href>/remote.php/dav/files/alice/a.txt</d:href>
    <d:propstat><d:status>HTTP/1.1 200 OK</d:status><d:prop>
      <d:getetag>&quot;e1&quot;</d:getetag>
      <d:getcontentlength>33</d:getcontentlength>
      <nc:upload_time>1790160705</nc:upload_time>
    </d:prop></d:propstat>
  </d:response>
  <d:response>
    <d:href>/remote.php/dav/files/alice/old.txt</d:href>
    <d:propstat><d:status>HTTP/1.1 200 OK</d:status><d:prop>
      <d:getetag>&quot;e2&quot;</d:getetag>
      <nc:upload_time>0</nc:upload_time>
    </d:prop></d:propstat>
  </d:response>
</d:multistatus>`
	var ms multistatus
	if err := xml.Unmarshal([]byte(body), &ms); err != nil {
		t.Fatal(err)
	}
	c := New("https://cloud.example.com", "alice", "pw")
	got := map[string]int64{}
	for _, r := range ms.Responses {
		e, ok, err := c.parseResponse(r)
		if err != nil || !ok {
			t.Fatalf("parse: ok=%v err=%v", ok, err)
		}
		got[e.Path] = e.UploadTime
	}
	if got["a.txt"] != 1790160705 {
		t.Errorf("a.txt UploadTime = %d, want 1790160705", got["a.txt"])
	}
	if got["old.txt"] != 0 {
		t.Errorf("old.txt UploadTime = %d, want 0 (server does not know it)", got["old.txt"])
	}
}

func TestContentKey(t *testing.T) {
	mt := time.Unix(1790160705, 0)
	if k := ContentKey(33, mt, 0); k != "" {
		t.Errorf("unknown upload time gave key %q, want empty (nothing to vouch for)", k)
	}
	a := ContentKey(33, mt, 1790160705)
	if a == "" {
		t.Fatal("key empty with a known upload time")
	}
	// Sub-second mtime noise (NTFS keeps 100ns, the server whole seconds) must
	// not make the same version look different.
	if b := ContentKey(33, mt.Add(400*time.Millisecond), 1790160705); b != a {
		t.Errorf("sub-second mtime changed the key: %q vs %q", b, a)
	}
	// A new upload of the same size and mtime is still a different version.
	if b := ContentKey(33, mt, 1790160846); b == a {
		t.Error("a later upload with the same size and mtime produced the same key")
	}
	if b := ContentKey(34, mt, 1790160705); b == a {
		t.Error("a size change produced the same key")
	}
	if b := ContentKey(33, mt.Add(time.Minute), 1790160705); b == a {
		t.Error("an mtime change produced the same key")
	}
}
