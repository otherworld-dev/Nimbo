package transport

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// Chunked upload v2: large files are uploaded as numbered chunks into a
// temporary upload collection under /remote.php/dav/uploads/<user>/<id>/, then
// assembled server-side with a single MOVE of the special ".file" pseudo-member
// to the destination. v2 requires a Destination header on every request and
// chunk names that are numbers (assembled in numeric order). See:
// https://docs.nextcloud.com/server/latest/developer_manual/client_apis/WebDAV/chunking.html

// uploadsURL builds an absolute URL under this account's uploads endpoint.
func (c *Client) uploadsURL(suffix string) string {
	return c.server + "/remote.php/dav/uploads/" + c.user + suffix
}

// CreateUpload creates the temporary upload collection for an upload session.
func (c *Client) CreateUpload(ctx context.Context, uploadID string) error {
	req, err := c.NewRequest(ctx, "MKCOL", c.uploadsURL("/"+uploadID), nil)
	if err != nil {
		return err
	}
	resp, err := c.DoOnce(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusMethodNotAllowed {
		return nil // created, or already exists (resuming)
	}
	return statusError("MKCOL upload", uploadID, resp)
}

// ListChunks returns the already-uploaded chunks for a session as a map of
// chunk name to size, enabling resume of an interrupted upload.
func (c *Client) ListChunks(ctx context.Context, uploadID string) (map[string]int64, error) {
	entries, err := c.propFindURL(ctx, c.uploadsURL("/"+uploadID), 1)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(entries))
	for _, e := range entries {
		name := lastSegment(e.Path)
		// Skip the collection itself and the ".file" assembly member.
		if name == "" || name == uploadID || name == ".file" {
			continue
		}
		out[name] = e.Size
	}
	return out, nil
}

// PutChunk uploads a single numbered chunk. destPath is the final files-root-
// relative destination, required by v2 in the Destination header.
func (c *Client) PutChunk(ctx context.Context, uploadID, chunkName string, body io.Reader, size int64, destPath string) error {
	req, err := c.NewRequest(ctx, http.MethodPut, c.uploadsURL("/"+uploadID+"/"+chunkName), body)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Destination", c.davURL(destPath))
	resp, err := c.DoOnce(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		return statusError("PUT chunk "+chunkName, uploadID, resp)
	}
	return nil
}

// AssembleUpload finalises a chunked upload by MOVEing the assembly member to
// destPath. OC-Total-Length lets the server check quota; an optional OC-Checksum
// has the server verify the assembled file. Returns the new ETag and file ID.
func (c *Client) AssembleUpload(ctx context.Context, uploadID, destPath string, totalLen int64, ocChecksum string) (etag, fileID string, err error) {
	req, err := c.NewRequest(ctx, "MOVE", c.uploadsURL("/"+uploadID+"/.file"), nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Destination", c.davURL(destPath))
	req.Header.Set("Overwrite", "T")
	req.Header.Set("OC-Total-Length", strconv.FormatInt(totalLen, 10))
	if ocChecksum != "" {
		req.Header.Set("OC-Checksum", ocChecksum)
	}
	resp, err := c.DoOnce(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		return "", "", statusError("MOVE assemble", destPath, resp)
	}
	etag, fileID = revisionHeaders(resp.Header)
	return etag, fileID, nil
}

// DeleteUpload removes a temporary upload collection (best-effort cleanup).
func (c *Client) DeleteUpload(ctx context.Context, uploadID string) error {
	req, err := c.NewRequest(ctx, http.MethodDelete, c.uploadsURL("/"+uploadID), nil)
	if err != nil {
		return err
	}
	resp, err := c.DoOnce(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// propFindURL is PropFind against an absolute URL (used for the uploads
// endpoint, which lives outside the files tree). It reuses the standard
// multistatus parsing but keys entries by their raw decoded href tail.
func (c *Client) propFindURL(ctx context.Context, url string, depth int) ([]Entry, error) {
	req, err := c.NewRequest(ctx, "PROPFIND", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Depth", strconv.Itoa(depth))
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("upload path not found")
	}
	if resp.StatusCode != http.StatusMultiStatus {
		return nil, statusError("PROPFIND", url, resp)
	}
	var ms multistatus
	if err := decodeXML(resp.Body, &ms); err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(ms.Responses))
	for _, r := range ms.Responses {
		href, err := unescapeHref(r.Href)
		if err != nil {
			continue
		}
		var prop *davProp
		for i := range r.Propstat {
			if contains200(r.Propstat[i].Status) {
				prop = &r.Propstat[i].Prop
				break
			}
		}
		if prop == nil {
			continue
		}
		size, _ := strconv.ParseInt(prop.GetContentLen, 10, 64)
		out = append(out, Entry{Path: href, Size: size, IsDir: prop.ResourceType.Collection != nil})
	}
	return out, nil
}
