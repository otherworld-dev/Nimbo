package transport

import (
	"encoding/xml"
	"io"
	"net/url"
	"strings"
)

// lastSegment returns the final non-empty path segment of a "/"-separated path.
func lastSegment(p string) string {
	p = strings.Trim(p, "/")
	if p == "" {
		return ""
	}
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// contains200 reports whether a WebDAV status line indicates HTTP 200.
func contains200(status string) bool {
	return strings.Contains(status, "200")
}

// decodeXML decodes an XML document from r into v.
func decodeXML(r io.Reader, v any) error {
	return xml.NewDecoder(r).Decode(v)
}

// pathSegmentEscape percent-encodes a single path segment for use in a URL,
// turning spaces into %20 and escaping reserved characters. Segments never
// contain "/" so the encoding is unambiguous.
func pathSegmentEscape(seg string) string {
	return url.PathEscape(seg)
}

// unescapeHref normalises a WebDAV <href>, which the server may return either
// as an absolute URL or as an absolute path, into a decoded path. The decoded
// path lets us strip the DAV base prefix to recover a files-root-relative path.
func unescapeHref(href string) (string, error) {
	u, err := url.Parse(href)
	if err != nil {
		return "", err
	}
	// u.Path is already percent-decoded by url.Parse.
	return u.Path, nil
}
