package transport

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
)

// favoritesReport is a WebDAV REPORT that asks the server for every file/folder
// the user has flagged as a favourite (oc:favorite = 1).
const favoritesReport = `<?xml version="1.0" encoding="UTF-8"?>
<oc:filter-files xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns" xmlns:nc="http://nextcloud.org/ns">
  <d:prop>
    <d:getetag/>
    <d:resourcetype/>
    <oc:fileid/>
    <oc:size/>
  </d:prop>
  <oc:filter-rules>
    <oc:favorite>1</oc:favorite>
  </oc:filter-rules>
</oc:filter-files>`

// Favorites returns the user's favourited files and folders (files-root-relative
// paths), newest-first ordering not guaranteed.
func (c *Client) Favorites(ctx context.Context) ([]Entry, error) {
	req, err := c.NewRequest(ctx, "REPORT", c.davURL(""), strings.NewReader(favoritesReport))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	req.Header.Set("Depth", "infinity")

	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMultiStatus {
		return nil, statusError("REPORT favorites", "/", resp)
	}

	var ms multistatus
	if err := xml.NewDecoder(resp.Body).Decode(&ms); err != nil {
		return nil, fmt.Errorf("decode favorites: %w", err)
	}
	prefix := c.davBase()
	entries := make([]Entry, 0, len(ms.Responses))
	for _, r := range ms.Responses {
		if e, ok := c.parseResponse(prefix, r); ok && e.Path != "" {
			entries = append(entries, e)
		}
	}
	return entries, nil
}
