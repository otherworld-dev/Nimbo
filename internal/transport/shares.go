package transport

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Nextcloud share types.
const (
	ShareTypeUser   = 0
	ShareTypeGroup  = 1
	ShareTypePublic = 3 // public link
	ShareTypeEmail  = 4
)

// Permission bits for shares.
const (
	PermRead   = 1
	PermUpdate = 2
	PermCreate = 4
	PermDelete = 8
	PermShare  = 16
	PermAll    = 31
)

// Share describes a file/folder share returned by the OCS Sharing API.
type Share struct {
	ID          flexString `json:"id"`
	ShareType   int        `json:"share_type"`
	Path        string     `json:"path"`
	Permissions int        `json:"permissions"`
	ShareWith   string     `json:"share_with"`
	URL         string     `json:"url"`        // public-link URL
	Token       string     `json:"token"`      // public-link token
	Expiration  string     `json:"expiration"` // YYYY-MM-DD or empty
}

// flexString decodes a JSON value that may be a string or a number into a string
// (Nextcloud's share id type varies by version).
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	*f = flexString(strings.Trim(string(b), `"`))
	return nil
}

func (f flexString) String() string { return string(f) }

const sharesPath = "apps/files_sharing/api/v1/shares"

// ListShares returns the shares on a path (files-root-relative), including
// reshares.
func (c *Client) ListShares(ctx context.Context, path string) ([]Share, error) {
	u := c.ocsURL(sharesPath) + "&path=" + url.QueryEscape("/"+strings.Trim(path, "/")) + "&reshares=true"
	var out []Share
	if err := c.doOCS(ctx, http.MethodGet, u, nil, "", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// PublicLinkOptions configures a public-link share.
type PublicLinkOptions struct {
	Password    string
	Permissions int    // 0 → server default (read)
	Expiration  string // YYYY-MM-DD, optional
}

// CreatePublicLink creates a public-link share for a path and returns it
// (Share.URL is the shareable link).
func (c *Client) CreatePublicLink(ctx context.Context, path string, opt PublicLinkOptions) (Share, error) {
	form := url.Values{
		"path":      {"/" + strings.Trim(path, "/")},
		"shareType": {strconv.Itoa(ShareTypePublic)},
	}
	if opt.Password != "" {
		form.Set("password", opt.Password)
	}
	if opt.Permissions > 0 {
		form.Set("permissions", strconv.Itoa(opt.Permissions))
	}
	if opt.Expiration != "" {
		form.Set("expireDate", opt.Expiration)
	}
	return c.createShare(ctx, form)
}

// CreateUserShare shares a path with another user.
func (c *Client) CreateUserShare(ctx context.Context, path, user string, permissions int) (Share, error) {
	if permissions <= 0 {
		permissions = PermRead
	}
	form := url.Values{
		"path":        {"/" + strings.Trim(path, "/")},
		"shareType":   {strconv.Itoa(ShareTypeUser)},
		"shareWith":   {user},
		"permissions": {strconv.Itoa(permissions)},
	}
	return c.createShare(ctx, form)
}

func (c *Client) createShare(ctx context.Context, form url.Values) (Share, error) {
	var sh Share
	err := c.doOCS(ctx, http.MethodPost, c.ocsURL(sharesPath),
		strings.NewReader(form.Encode()), "application/x-www-form-urlencoded", &sh)
	return sh, err
}

// DeleteShare removes a share by ID.
func (c *Client) DeleteShare(ctx context.Context, id string) error {
	u := c.ocsURL(sharesPath + "/" + id)
	return c.doOCS(ctx, http.MethodDelete, u, nil, "", nil)
}
