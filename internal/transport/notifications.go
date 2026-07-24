package transport

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// Notification is a single entry from the Nextcloud notifications app (shares,
// Talk mentions, app messages, etc.).
type Notification struct {
	ID      int                  `json:"notification_id"`
	App     string               `json:"app"`
	Subject string               `json:"subject"`
	Message string               `json:"message"`
	Link    string               `json:"link"`
	Object  string               `json:"object_type"`
	Time    string               `json:"datetime"`
	Actions []NotificationAction `json:"actions"`
}

// NotificationAction is a button on a notification (e.g. Accept / Decline a
// share). Type is the HTTP method to call Link with; Primary marks the suggested
// default action.
type NotificationAction struct {
	Label   string `json:"label"`
	Link    string `json:"link"`
	Type    string `json:"type"`
	Primary bool   `json:"primary"`
}

// Notifications fetches the current notifications for the account via the OCS
// notifications API. An empty list (and nil error) is normal.
func (c *Client) Notifications(ctx context.Context) ([]Notification, error) {
	var out []Notification
	if err := c.getOCS(ctx, "apps/notifications/api/v2/notifications", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// DismissNotification removes a single notification by ID. A 404 (already gone)
// is treated as success.
func (c *Client) DismissNotification(ctx context.Context, id int) error {
	url := c.server + "/ocs/v2.php/apps/notifications/api/v2/notifications/" + strconv.Itoa(id)
	return c.ocsAction(ctx, http.MethodDelete, url)
}

// DismissAllNotifications clears every notification for the account in a single
// request (the OCS collection-delete endpoint). A 404 (nothing to clear) is
// treated as success.
func (c *Client) DismissAllNotifications(ctx context.Context) error {
	url := c.server + "/ocs/v2.php/apps/notifications/api/v2/notifications"
	return c.ocsAction(ctx, http.MethodDelete, url)
}

// ExecuteAction performs a notification action (Accept/Decline/etc.) by calling
// its link with its method. The link may be absolute or server-relative.
func (c *Client) ExecuteAction(ctx context.Context, a NotificationAction) error {
	method := strings.ToUpper(a.Type)
	if method == "" {
		method = http.MethodGet
	}
	url := a.Link
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = c.server + url
	}
	return c.ocsAction(ctx, method, url)
}

// ocsAction issues an authenticated OCS request with no response body of
// interest, validating the HTTP status (the OCS envelope's own code is not
// always present for action endpoints).
func (c *Client) ocsAction(ctx context.Context, method, url string) error {
	req, err := c.NewRequest(ctx, method, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("OCS-APIRequest", "true")
	req.Header.Set("Accept", "application/json")

	resp, err := c.DoOnce(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil // already handled/dismissed elsewhere
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: server returned %s", method, url, resp.Status)
	}
	return nil
}
