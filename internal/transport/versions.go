package transport

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// FileVersion is a previous revision of a file held by Nextcloud's versions app.
type FileVersion struct {
	Href     string    // DAV href, used to restore or download
	Modified time.Time // when this version was created
	Size     int64
}

func (c *Client) versionsBase() string { return "/remote.php/dav/versions/" + c.user }

const versionsPropfind = `<?xml version="1.0" encoding="UTF-8"?>
<d:propfind xmlns:d="DAV:">
  <d:prop>
    <d:getlastmodified/>
    <d:getcontentlength/>
  </d:prop>
</d:propfind>`

// ListVersions returns the stored versions of a file (by its oc:fileid), newest
// first. An empty result means the file has no prior versions.
func (c *Client) ListVersions(ctx context.Context, fileID string) ([]FileVersion, error) {
	if fileID == "" {
		return nil, nil
	}
	base := c.versionsBase() + "/versions/" + fileID
	responses, err := c.propfindRaw(ctx, c.server+base, 1, versionsPropfind)
	if err != nil {
		return nil, err
	}
	var out []FileVersion
	for _, r := range responses {
		href := strings.TrimRight(r.Href, "/")
		if strings.HasSuffix(href, "/"+fileID) { // the collection itself
			continue
		}
		var p *davProp
		for i := range r.Propstat {
			if strings.Contains(r.Propstat[i].Status, "200") {
				p = &r.Propstat[i].Prop
				break
			}
		}
		if p == nil {
			continue
		}
		v := FileVersion{Href: r.Href}
		v.Size, _ = strconv.ParseInt(strings.TrimSpace(p.GetContentLen), 10, 64)
		if t, err := http.ParseTime(p.GetLastModified); err == nil {
			v.Modified = t
		}
		out = append(out, v)
	}
	return out, nil
}

// RestoreVersion makes the given version the current file content.
func (c *Client) RestoreVersion(ctx context.Context, versionHref string) error {
	dst := c.server + c.versionsBase() + "/restore/target"
	return c.moveURL(ctx, c.server+versionHref, dst)
}
