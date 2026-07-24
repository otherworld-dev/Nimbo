package transport

import (
	"encoding/xml"
	"testing"
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

func TestServerReadOnly(t *testing.T) {
	cases := []struct {
		perm string
		dir  bool
		want bool
	}{
		{"MG", true, true},      // .Collectives root: mounted+read, no create -> read-only
		{"RMGCK", true, false},  // a collective: can create file/folder -> writable
		{"RGDNVW", false, false}, // normal file: has W -> writable
		{"RG", false, true},     // read-only shared file: no W -> read-only
		{"", true, false},       // unknown -> treat as writable (never wrongly lock)
		{"", false, false},
	}
	for _, c := range cases {
		e := Entry{Permissions: c.perm, IsDir: c.dir}
		if got := e.ServerReadOnly(); got != c.want {
			t.Errorf("ServerReadOnly(perm=%q dir=%v) = %v, want %v", c.perm, c.dir, got, c.want)
		}
	}
}
