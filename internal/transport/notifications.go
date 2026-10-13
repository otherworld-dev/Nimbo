package transport

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
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

// IsWeb reports whether the action is a link for the user to open in a browser
// (type WEB in the notifications API) rather than a request for the client to
// send. Suspicious-login warnings carry one pointing at an IP lookup site.
func (a NotificationAction) IsWeb() bool { return strings.EqualFold(a.Type, "WEB") }

// DeleteAppPassword revokes the app password this client signs in with
// (Nextcloud's core/apppassword endpoint), for one a sign-in produced that the
// caller isn't keeping. A 404 (already gone) counts as done.
func (c *Client) DeleteAppPassword(ctx context.Context) error {
	return c.ocsAction(ctx, http.MethodDelete, c.server+"/ocs/v2.php/core/apppassword")
}

// ExecuteAction performs a notification action (Accept/Decline/etc.) by calling
// its link with its method. The link may be absolute or server-relative.
//
// The request carries the account's login, so it is only ever sent to the
// account's own server: a link to any other host is refused, as is a WEB
// action, which the UI opens in the browser instead. Before GitHub #20 a WEB
// action went out as a "WEB" request to the site it named, login attached.
func (c *Client) ExecuteAction(ctx context.Context, a NotificationAction) error {
	if a.IsWeb() {
		return errors.New("notification action: a WEB action is opened in the browser, not sent")
	}
	method := strings.ToUpper(a.Type)
	if method == "" {
		method = http.MethodGet
	}
	target := a.Link
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		target = c.server + target
	}
	if !c.sameServer(target) {
		return fmt.Errorf("notification action: refusing to send the login to %s, which is not this account's server", hostOf(target))
	}
	return c.ocsAction(ctx, method, target)
}

// sameServer reports whether target is on the account's server: same scheme
// and host (port included) as the configured server URL.
func (c *Client) sameServer(target string) bool {
	t, err := url.Parse(target)
	if err != nil {
		return false
	}
	s, err := url.Parse(c.server)
	if err != nil {
		return false
	}
	return strings.EqualFold(t.Scheme, s.Scheme) && strings.EqualFold(t.Host, s.Host)
}

func hostOf(target string) string {
	if u, err := url.Parse(target); err == nil && u.Host != "" {
		return u.Host
	}
	return "another host"
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
