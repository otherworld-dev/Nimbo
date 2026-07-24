package transport

import (
	"context"
	"path"
	"strconv"
	"strings"
	"time"
)

// TrashItem is a deleted file/folder in the Nextcloud trashbin.
type TrashItem struct {
	Href             string    // DAV href/path, used to restore or delete
	Name             string    // original filename
	OriginalLocation string    // files-root-relative path it was deleted from
	DeletedAt        time.Time
	Size             int64
	IsDir            bool
}

func (c *Client) trashBase() string { return "/remote.php/dav/trashbin/" + c.user }

const trashPropfind = `<?xml version="1.0" encoding="UTF-8"?>
<d:propfind xmlns:d="DAV:" xmlns:nc="http://nextcloud.org/ns">
  <d:prop>
    <nc:trashbin-filename/>
    <nc:trashbin-original-location/>
    <nc:trashbin-deletion-time/>
    <d:getcontentlength/>
    <d:resourcetype/>
  </d:prop>
</d:propfind>`

// ListTrash returns the items currently in the trashbin (newest first).
func (c *Client) ListTrash(ctx context.Context) ([]TrashItem, error) {
	base := c.trashBase() + "/trash"
	responses, err := c.propfindRaw(ctx, c.server+base, 1, trashPropfind)
	if err != nil {
		return nil, err
	}
	var out []TrashItem
	for _, r := range responses {
		href := r.Href
		// Skip the trash collection root itself.
		if strings.TrimRight(href, "/") == strings.TrimRight(base, "/") ||
			strings.HasSuffix(strings.TrimRight(href, "/"), "/trash") {
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
		it := TrashItem{
			Href:             href,
			Name:             p.TrashFilename,
			OriginalLocation: p.TrashOrigLoc,
			IsDir:            p.ResourceType.Collection != nil,
		}
		if it.Name == "" {
			it.Name = path.Base(strings.TrimRight(href, "/"))
		}
		it.Size, _ = strconv.ParseInt(strings.TrimSpace(p.GetContentLen), 10, 64)
		if ts, err := strconv.ParseInt(strings.TrimSpace(p.TrashDelTime), 10, 64); err == nil && ts > 0 {
			it.DeletedAt = time.Unix(ts, 0)
		}
		out = append(out, it)
	}
	return out, nil
}

// RestoreTrash restores a trashed item (by its href) to its original location.
func (c *Client) RestoreTrash(ctx context.Context, href string) error {
	dst := c.server + c.trashBase() + "/restore/" + path.Base(strings.TrimRight(href, "/"))
	return c.moveURL(ctx, c.server+href, dst)
}

// DeleteTrash permanently removes a trashed item.
func (c *Client) DeleteTrash(ctx context.Context, href string) error {
	return c.deleteURL(ctx, c.server+href)
}
