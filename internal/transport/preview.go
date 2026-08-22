package transport

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// previewMaxBytes caps what we will hold in memory for one thumbnail. Previews
// are small by construction (a few hundred KB at most), so anything larger means
// the server handed us something other than a preview — a login page, an error
// document — and reading it all would be pointless.
const previewMaxBytes = 8 << 20 // 8 MiB

// Preview fetches a server-rendered thumbnail for the file with the given
// Nextcloud file id, at most px pixels on each side.
//
// Nextcloud renders previews for images, PDFs and office documents from this one
// endpoint, so callers do not need to know which types are previewable — an
// un-previewable file simply answers with an error status.
//
// The aspect ratio is preserved (a=1) rather than cropping to a square, so the
// caller decides how to fit the result.
func (c *Client) Preview(ctx context.Context, fileID string, px int) ([]byte, error) {
	if fileID == "" {
		return nil, errors.New("preview: file id is required")
	}
	if px <= 0 {
		px = 256
	}
	q := url.Values{}
	q.Set("fileId", fileID)
	q.Set("x", strconv.Itoa(px))
	q.Set("y", strconv.Itoa(px))
	q.Set("a", "1")

	req, err := c.NewRequest(ctx, http.MethodGet, c.server+"/index.php/core/preview?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req) // idempotent GET: the retrying client is appropriate
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, statusError("preview", fileID, resp)
	}
	return io.ReadAll(io.LimitReader(resp.Body, previewMaxBytes))
}
