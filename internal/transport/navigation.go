package transport

import "context"

// App is an entry from the Nextcloud app navigation menu (Files, Photos, Talk,
// Notes, …) — what the user sees in the top bar of the web UI.
type App struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Href string `json:"href"`
	Icon string `json:"icon"`
}

// NavigationApps fetches the user's app menu via the OCS core navigation API.
func (c *Client) NavigationApps(ctx context.Context) ([]App, error) {
	var out []App
	if err := c.getOCS(ctx, "core/navigation/apps", &out); err != nil {
		return nil, err
	}
	return out, nil
}
