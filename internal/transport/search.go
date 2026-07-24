package transport

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

// SearchResult is one hit from Nextcloud's unified search (files provider).
type SearchResult struct {
	Title       string `json:"title"`       // file name
	Subline     string `json:"subline"`     // its folder / path
	ResourceURL string `json:"resourceUrl"` // URL to open it in the web UI (may be relative)
	Icon        string `json:"icon"`
}

// SearchFiles queries the server's unified search "files" provider for term and
// returns up to limit matches (by name and, where the server supports it,
// content). Returns nil with no error when search isn't available.
func (c *Client) SearchFiles(ctx context.Context, term string, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 20
	}
	u := c.ocsURL("search/providers/files/search") +
		"&term=" + url.QueryEscape(term) + "&limit=" + strconv.Itoa(limit)
	var data struct {
		Entries []SearchResult `json:"entries"`
	}
	if err := c.doOCS(ctx, http.MethodGet, u, nil, "", &data); err != nil {
		return nil, err
	}
	return data.Entries, nil
}
