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
` + entryProps + `  </d:prop>
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
	entries := make([]Entry, 0, len(ms.Responses))
	for _, r := range ms.Responses {
		e, ok, err := c.parseResponse(r)
		if err != nil {
			return nil, fmt.Errorf("favorites: %w", err)
		}
		if ok && e.Path != "" {
			entries = append(entries, e)
		}
	}
	return entries, nil
}

// setFavoriteBody is a PROPPATCH that sets oc:favorite. Nextcloud treats the
// property as a flag: "1" stars the file, "0" unstars it (there is no removal
// form — sending 0 is how you take the star away).
const setFavoriteBody = `<?xml version="1.0" encoding="UTF-8"?>
<d:propertyupdate xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns">
  <d:set><d:prop><oc:favorite>%s</oc:favorite></d:prop></d:set>
</d:propertyupdate>`

// SetFavorite stars or unstars one file or folder (files-root-relative path).
//
// The account root cannot be favourited: the server answers that attempt with
// an error that reads like a failure of the whole request, so we refuse it here
// where the reason can be stated plainly.
func (c *Client) SetFavorite(ctx context.Context, remotePath string, fav bool) error {
	clean := strings.Trim(strings.TrimSpace(remotePath), "/")
	if clean == "" {
		return fmt.Errorf("set favorite: the account root cannot be favourited")
	}
	value := "0"
	if fav {
		value = "1"
	}
	body := fmt.Sprintf(setFavoriteBody, value)
	req, err := c.NewRequest(ctx, "PROPPATCH", c.davURL(clean), strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")

	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMultiStatus && resp.StatusCode != http.StatusOK {
		return statusError("PROPPATCH favorite", clean, resp)
	}
	return nil
}
