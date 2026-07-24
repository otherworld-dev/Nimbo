package transport

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

// UserStatusInfo is the user's Nextcloud presence (from the user_status app).
type UserStatusInfo struct {
	Status  string `json:"status"`  // online | away | dnd | invisible | offline
	Message string `json:"message"` // custom status message, if set
	Icon    string `json:"icon"`    // custom status emoji, if set
}

// UserStatus fetches the current user's Nextcloud status. Returns a zero value
// (no error surfaced to callers that ignore it) when the user_status app is
// disabled or no status is set.
func (c *Client) UserStatus(ctx context.Context) (UserStatusInfo, error) {
	var out UserStatusInfo
	if err := c.getOCS(ctx, "apps/user_status/api/v1/user_status", &out); err != nil {
		return UserStatusInfo{}, err
	}
	return out, nil
}

const userStatusBase = "apps/user_status/api/v1/user_status"

// SetUserStatusType sets the presence (online | away | dnd | invisible).
func (c *Client) SetUserStatusType(ctx context.Context, statusType string) error {
	form := url.Values{"statusType": {statusType}}
	return c.doOCS(ctx, http.MethodPut, c.ocsURL(userStatusBase+"/status"),
		strings.NewReader(form.Encode()), "application/x-www-form-urlencoded", nil)
}

// SetUserStatusMessage sets a custom status message (and optional emoji icon).
func (c *Client) SetUserStatusMessage(ctx context.Context, message, icon string) error {
	form := url.Values{"message": {message}}
	if icon != "" {
		form.Set("statusIcon", icon)
	}
	return c.doOCS(ctx, http.MethodPut, c.ocsURL(userStatusBase+"/message/custom"),
		strings.NewReader(form.Encode()), "application/x-www-form-urlencoded", nil)
}

// ClearUserStatusMessage removes the custom status message.
func (c *Client) ClearUserStatusMessage(ctx context.Context) error {
	return c.doOCS(ctx, http.MethodDelete, c.ocsURL(userStatusBase+"/message"), nil, "", nil)
}
