package transport

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// rootIDBody asks for oc:id only (see davProp.ID).
const rootIDBody = `<?xml version="1.0" encoding="UTF-8"?>
<d:propfind xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns"><d:prop><oc:id/></d:prop></d:propfind>`

// RootID returns the oc:id of this account's DAV root — a value that
// identifies both the Nextcloud instance and the user's home folder. One
// PROPFIND, sent exactly once with no retry loop: the local-route prober calls
// this on a short deadline and wants a fast, honest answer.
func (c *Client) RootID(ctx context.Context) (string, error) {
	u := c.davURL("")
	req, err := c.NewRequest(ctx, "PROPFIND", u, strings.NewReader(rootIDBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	req.Header.Set("Depth", "0")
	resp, err := c.send(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMultiStatus {
		return "", statusError("PROPFIND root id", u, resp)
	}
	var ms multistatus
	if err := xml.NewDecoder(resp.Body).Decode(&ms); err != nil {
		return "", fmt.Errorf("decode PROPFIND root id: %w", err)
	}
	for _, r := range ms.Responses {
		for _, ps := range r.Propstat {
			if strings.Contains(ps.Status, "200") && ps.Prop.ID != "" {
				return ps.Prop.ID, nil
			}
		}
	}
	return "", errors.New("PROPFIND root id: no oc:id in response")
}
