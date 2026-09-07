package transport

import (
	"context"
	"fmt"
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
	ID           flexString `json:"id"`
	ShareType    int        `json:"share_type"`
	Path         string     `json:"path"`
	ItemType     string     `json:"item_type"` // "file" or "folder"
	Permissions  int        `json:"permissions"`
	ShareWith    string     `json:"share_with"`
	URL          string     `json:"url"`               // public-link URL
	Token        string     `json:"token"`             // public-link token
	Expiration   string     `json:"expiration"`        // YYYY-MM-DD or empty
	Owner        string     `json:"uid_owner"`         // who shared it
	OwnerDisplay string     `json:"displayname_owner"` // their display name
}

// SharedBy names the sharer as a human would.
func (s Share) SharedBy() string {
	if s.OwnerDisplay != "" {
		return s.OwnerDisplay
	}
	if s.Owner != "" {
		return s.Owner
	}
	return "Someone"
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

// ListAllShares returns every share this account takes part in, in two
// halves: the shares the user created (any type), and the files/folders other
// people shared WITH them. Paths are files-root-relative, as the user sees
// them.
func (c *Client) ListAllShares(ctx context.Context) (own, received []Share, err error) {
	if err := c.doOCS(ctx, http.MethodGet, c.ocsURL(sharesPath), nil, "", &own); err != nil {
		return nil, nil, err
	}
	if err := c.doOCS(ctx, http.MethodGet, c.ocsURL(sharesPath)+"&shared_with_me=true", nil, "", &received); err != nil {
		return nil, nil, err
	}
	return own, received, nil
}

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

// sharePath validates the target of a new share. The account root is refused:
// "" and "/" both resolve to everything the user owns, and one mistyped path
// should not be able to publish an entire account behind a single link.
func sharePath(path string) (string, error) {
	clean := strings.Trim(strings.TrimSpace(path), "/")
	if clean == "" {
		return "", fmt.Errorf("create share: refusing to share the account root")
	}
	return clean, nil
}

// CreatePublicLink creates a public-link share for a path and returns it
// (Share.URL is the shareable link).
func (c *Client) CreatePublicLink(ctx context.Context, path string, opt PublicLinkOptions) (Share, error) {
	clean, err := sharePath(path)
	if err != nil {
		return Share{}, err
	}
	form := url.Values{
		"path":      {"/" + clean},
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
	clean, err := sharePath(path)
	if err != nil {
		return Share{}, err
	}
	recipient := strings.TrimSpace(user)
	if recipient == "" {
		// Some server versions accept this and record a share with nobody,
		// which then sits in the user's share list unexplainable.
		return Share{}, fmt.Errorf("create share: no one to share with")
	}
	if permissions <= 0 {
		permissions = PermRead
	}
	form := url.Values{
		"path":        {"/" + clean},
		"shareType":   {strconv.Itoa(ShareTypeUser)},
		"shareWith":   {recipient},
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
	clean := strings.Trim(strings.TrimSpace(id), "/")
	if clean == "" {
		// An empty id builds ".../shares/" — a DELETE aimed at the shares
		// collection rather than at one share.
		return fmt.Errorf("delete share: no share id")
	}
	u := c.ocsURL(sharesPath + "/" + clean)
	return c.doOCS(ctx, http.MethodDelete, u, nil, "", nil)
}
