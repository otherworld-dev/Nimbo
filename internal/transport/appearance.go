package transport

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ThemeAppearance reports the user's enabled Nextcloud appearance: "dark",
// "light", or "default" (the user follows the system/OS appearance).
//
// Nextcloud exposes no OCS/REST endpoint for the per-user enabled theme, so we
// read the appearance the web UI renders: every authenticated page sets a
// boolean attribute on <body> for each enabled theme (data-theme-dark /
// data-theme-light / data-theme-default, plus high-contrast variants that still
// contain "dark"/"light").
func (c *Client) ThemeAppearance(ctx context.Context) (string, error) {
	req, err := c.NewRequest(ctx, http.MethodGet, c.server+"/index.php/apps/files/", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "text/html")
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("theme page: status %d", resp.StatusCode)
	}
	// The <body …> tag sits just after </head>; cap the read so we don't pull the
	// whole app payload.
	data, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return "", err
	}
	i := strings.Index(string(data), "<body")
	if i < 0 {
		return "", fmt.Errorf("theme page: no <body> tag")
	}
	tag := string(data[i:])
	if j := strings.IndexByte(tag, '>'); j >= 0 {
		tag = tag[:j]
	}
	switch {
	case strings.Contains(tag, "data-theme-dark"):
		return "dark", nil
	case strings.Contains(tag, "data-theme-light"):
		return "light", nil
	default:
		return "default", nil
	}
}
