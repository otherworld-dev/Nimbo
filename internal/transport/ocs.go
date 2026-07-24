package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// OCS (Open Collaboration Services) endpoints wrap their payload in a standard
// envelope and require the OCS-APIRequest header. These helpers handle both.

// ocsEnvelope is the common wrapper around every OCS response.
type ocsEnvelope struct {
	OCS struct {
		Meta struct {
			Status     string `json:"status"`
			StatusCode int    `json:"statuscode"`
			Message    string `json:"message"`
		} `json:"meta"`
		Data json.RawMessage `json:"data"`
	} `json:"ocs"`
}

// ocsURL builds an absolute OCS v2 URL for the given path (e.g.
// "cloud/capabilities"), always requesting JSON.
func (c *Client) ocsURL(path string) string {
	return c.server + "/ocs/v2.php/" + path + "?format=json"
}

// getOCS performs an authenticated OCS GET and unmarshals the inner data field
// into out. It validates the OCS envelope status.
func (c *Client) getOCS(ctx context.Context, path string, out any) error {
	return c.doOCS(ctx, http.MethodGet, c.ocsURL(path), nil, "", out)
}

// doOCS performs an authenticated OCS request to an absolute URL (which may
// carry its own query string), decoding the inner data into out. GET uses the
// retrying client; other methods are issued once.
func (c *Client) doOCS(ctx context.Context, method, fullURL string, body io.Reader, contentType string, out any) error {
	req, err := c.NewRequest(ctx, method, fullURL, body)
	if err != nil {
		return err
	}
	req.Header.Set("OCS-APIRequest", "true")
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	var resp *http.Response
	if method == http.MethodGet {
		resp, err = c.Do(req)
	} else {
		resp, err = c.DoOnce(req)
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("read OCS response: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("OCS %s: unauthorized (app password rejected)", fullURL)
	}

	var env ocsEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("decode OCS envelope: %w", err)
	}
	if sc := env.OCS.Meta.StatusCode; sc != 100 && sc != 200 {
		return fmt.Errorf("OCS request failed: %s (code %d)", env.OCS.Meta.Message, sc)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.OCS.Data, out); err != nil {
		return fmt.Errorf("decode OCS data: %w", err)
	}
	return nil
}
