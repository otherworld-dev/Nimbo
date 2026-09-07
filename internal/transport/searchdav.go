package transport

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// SearchByName finds files and folders whose name contains term, anywhere in
// the user's account, and returns them as ordinary Entry values.
//
// This exists alongside SearchFiles (the OCS unified search) because the two
// answer different questions. Unified search returns display text and a web-UI
// URL — good for "show me this in a browser", useless for a file manager,
// which needs a path it can open. WebDAV SEARCH returns real hrefs, so a hit
// behaves like any other row.
//
// The trade is that this matches names only: the unified search can reach file
// CONTENTS where the server indexes them, and this cannot.
func (c *Client) SearchByName(ctx context.Context, term string, limit int) ([]Entry, error) {
	clean := strings.TrimSpace(term)
	if clean == "" {
		// A blank LIKE pattern matches the entire account. That would not fail;
		// it would look like a working search that found everything.
		return nil, fmt.Errorf("search: no search term")
	}
	if limit <= 0 {
		limit = 50
	}

	body := fmt.Sprintf(searchRequest, entryProps, xmlEscape(c.user), likePattern(clean), strconv.Itoa(limit))
	req, err := c.NewRequest(ctx, "SEARCH", c.server+"/remote.php/dav/", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "text/xml; charset=utf-8")

	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMultiStatus {
		return nil, statusError("SEARCH", clean, resp)
	}

	var ms multistatus
	if err := xml.NewDecoder(resp.Body).Decode(&ms); err != nil {
		return nil, fmt.Errorf("decode search results: %w", err)
	}
	entries := make([]Entry, 0, len(ms.Responses))
	for _, r := range ms.Responses {
		e, ok, err := c.parseResponse(r)
		if err != nil {
			return nil, fmt.Errorf("search results: %w", err)
		}
		if ok && e.Path != "" {
			entries = append(entries, e)
		}
	}
	return entries, nil
}

// searchRequest is RFC 5323 basicsearch. The scope is /files/<user> — relative
// to the DAV root, which is where SEARCH is served, not to the files
// collection. Verbs are: properties, user, LIKE literal, result limit.
const searchRequest = `<?xml version="1.0" encoding="UTF-8"?>
<d:searchrequest xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns" xmlns:nc="http://nextcloud.org/ns">
  <d:basicsearch>
    <d:select>
      <d:prop>
%s      </d:prop>
    </d:select>
    <d:from>
      <d:scope><d:href>/files/%s</d:href><d:depth>infinity</d:depth></d:scope>
    </d:from>
    <d:where>
      <d:like>
        <d:prop><d:displayname/></d:prop>
        <d:literal>%s</d:literal>
      </d:like>
    </d:where>
    <d:orderby/>
    <d:limit><d:nresults>%s</d:nresults></d:limit>
  </d:basicsearch>
</d:searchrequest>`

// likePattern wraps a user's term for a SQL LIKE, escaping the wildcards first.
// A typed "%" must match a literal percent sign, not "anything at all" — the
// latter would quietly turn a narrow search into a listing of the whole
// account.
func likePattern(term string) string {
	escaped := strings.NewReplacer(`\`, `\`, `%`, `\%`, `_`, `\_`).Replace(term)
	return xmlEscape("%" + escaped + "%")
}

// xmlEscape makes a string safe to place in element text. A term containing
// "<" or "&" would otherwise produce a malformed request body.
func xmlEscape(s string) string {
	var b strings.Builder
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		return ""
	}
	return b.String()
}
