package transport

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Entry describes a single file or directory returned by a PROPFIND. Paths are
// expressed relative to the account's WebDAV files root, using "/" separators
// and no leading slash (the root itself is "").
type Entry struct {
	Path         string
	IsDir        bool
	Size         int64
	ETag         string
	FileID       string // oc:fileid — stable across renames/moves
	LastModified time.Time
	ContentType  string
	Checksums    string // raw oc:checksums value, e.g. "SHA1:abc MD5:def"
	IsEncrypted  bool   // nc:is-encrypted — an end-to-end encrypted folder (contents are opaque to clients without E2EE keys)
	Permissions  string // oc:permissions, e.g. "RGDNVW" (file) / "RMGCK" (dir); empty = unknown
}

// ServerReadOnly reports whether the server marks this entry as not writable: a
// file with no W(rite) permission, or a directory you cannot create in (no
// C(reate file) or K(create folder)). Empty permissions (unknown) is treated as
// writable so normal files are never wrongly locked.
func (e Entry) ServerReadOnly() bool {
	if e.Permissions == "" {
		return false
	}
	if e.IsDir {
		return !strings.ContainsAny(e.Permissions, "CK")
	}
	return !strings.Contains(e.Permissions, "W")
}

// davBase is the path prefix for this account's files endpoint.
func (c *Client) davBase() string {
	return "/remote.php/dav/files/" + c.user
}

// davURL builds an absolute WebDAV URL for a files-root-relative path.
func (c *Client) davURL(remotePath string) string {
	return c.server + c.davBase() + escapePath(remotePath)
}

// escapePath percent-encodes each segment of a "/"-separated path, preserving
// the separators and a single leading slash.
func escapePath(p string) string {
	p = strings.TrimLeft(p, "/")
	if p == "" {
		return "/"
	}
	parts := strings.Split(p, "/")
	for i, seg := range parts {
		parts[i] = pathSegmentEscape(seg)
	}
	return "/" + strings.Join(parts, "/")
}

// propfindBody requests exactly the properties Entry exposes.
const propfindBody = `<?xml version="1.0" encoding="UTF-8"?>
<d:propfind xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns" xmlns:nc="http://nextcloud.org/ns">
  <d:prop>
    <d:getetag/>
    <d:getlastmodified/>
    <d:getcontentlength/>
    <d:getcontenttype/>
    <d:resourcetype/>
    <oc:fileid/>
    <oc:size/>
    <oc:checksums/>
    <oc:permissions/>
    <nc:is-encrypted/>
  </d:prop>
</d:propfind>`

// multistatus mirrors the WebDAV PROPFIND XML response.
type multistatus struct {
	XMLName   xml.Name      `xml:"multistatus"`
	Responses []davResponse `xml:"response"`
}

type davResponse struct {
	Href     string        `xml:"href"`
	Propstat []davPropstat `xml:"propstat"`
}

type davPropstat struct {
	Status string  `xml:"status"`
	Prop   davProp `xml:"prop"`
}

type davProp struct {
	GetETag         string `xml:"getetag"`
	GetLastModified string `xml:"getlastmodified"`
	GetContentLen   string `xml:"getcontentlength"`
	GetContentType  string `xml:"getcontenttype"`
	ResourceType    struct {
		Collection *struct{} `xml:"collection"`
	} `xml:"resourcetype"`
	FileID      string `xml:"fileid"`
	OCSize      string `xml:"size"`
	IsEncrypted string `xml:"is-encrypted"`
	Permissions string `xml:"permissions"`
	// oc:checksums wraps one or more <oc:checksum> children; the digest text is
	// in the child, not the wrapper.
	Checksums string `xml:"checksums>checksum"`
	// Trashbin properties (nc namespace; only populated for trashbin PROPFINDs).
	TrashFilename string `xml:"trashbin-filename"`
	TrashOrigLoc  string `xml:"trashbin-original-location"`
	TrashDelTime  string `xml:"trashbin-deletion-time"`
}

// propfindRaw runs a PROPFIND against an absolute URL with a custom body and
// returns the raw responses (used for the trashbin and versions endpoints,
// which live outside the files DAV tree).
func (c *Client) propfindRaw(ctx context.Context, fullURL string, depth int, body string) ([]davResponse, error) {
	req, err := c.NewRequest(ctx, "PROPFIND", fullURL, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	req.Header.Set("Depth", strconv.Itoa(depth))
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMultiStatus {
		return nil, statusError("PROPFIND", fullURL, resp)
	}
	var ms multistatus
	if err := xml.NewDecoder(resp.Body).Decode(&ms); err != nil {
		return nil, fmt.Errorf("decode PROPFIND: %w", err)
	}
	return ms.Responses, nil
}

// deleteURL issues a WebDAV DELETE against an absolute URL.
func (c *Client) deleteURL(ctx context.Context, fullURL string) error {
	req, err := c.NewRequest(ctx, http.MethodDelete, fullURL, nil)
	if err != nil {
		return err
	}
	resp, err := c.DoOnce(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return statusError("DELETE", fullURL, resp)
	}
	return nil
}

// moveURL issues a WebDAV MOVE between two absolute URLs.
func (c *Client) moveURL(ctx context.Context, srcURL, dstURL string) error {
	req, err := c.NewRequest(ctx, "MOVE", srcURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Destination", dstURL)
	resp, err := c.DoOnce(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		return statusError("MOVE", srcURL, resp)
	}
	return nil
}

// PropFind lists the entry at remotePath. depth 0 returns just that entry;
// depth 1 returns it plus its immediate children. The entry's own row is
// included first when present.
func (c *Client) PropFind(ctx context.Context, remotePath string, depth int) ([]Entry, error) {
	return c.propFind(ctx, remotePath, strconv.Itoa(depth))
}

// PropFindRecursive returns remotePath and its entire subtree in one request
// (Depth: infinity). On a large account this replaces thousands of per-directory
// PROPFINDs with a few bulk ones — but the response can be large, so use it only
// for subtrees being fully enumerated (e.g. an initial clone), not routine syncs.
// Servers may refuse Depth: infinity (HTTP 403/507); callers should fall back.
func (c *Client) PropFindRecursive(ctx context.Context, remotePath string) ([]Entry, error) {
	return c.propFind(ctx, remotePath, "infinity")
}

func (c *Client) propFind(ctx context.Context, remotePath, depth string) ([]Entry, error) {
	req, err := c.NewRequest(ctx, "PROPFIND", c.davURL(remotePath), strings.NewReader(propfindBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	req.Header.Set("Depth", depth)

	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("path %q not found", remotePath)
	}
	if resp.StatusCode != http.StatusMultiStatus {
		return nil, statusError("PROPFIND", remotePath, resp)
	}

	var ms multistatus
	if err := xml.NewDecoder(resp.Body).Decode(&ms); err != nil {
		return nil, fmt.Errorf("decode PROPFIND response: %w", err)
	}

	prefix := c.davBase()
	entries := make([]Entry, 0, len(ms.Responses))
	for _, r := range ms.Responses {
		e, ok := c.parseResponse(prefix, r)
		if ok {
			entries = append(entries, e)
		}
	}
	return entries, nil
}

// parseResponse converts a single PROPFIND <response> into an Entry, selecting
// the 200-status propstat block. ok is false if the row should be skipped.
func (c *Client) parseResponse(prefix string, r davResponse) (Entry, bool) {
	href, err := unescapeHref(r.Href)
	if err != nil {
		return Entry{}, false
	}
	rel := strings.TrimPrefix(href, prefix)
	rel = strings.Trim(rel, "/")

	var prop *davProp
	for i := range r.Propstat {
		if strings.Contains(r.Propstat[i].Status, "200") {
			prop = &r.Propstat[i].Prop
			break
		}
	}
	if prop == nil {
		return Entry{}, false
	}

	e := Entry{
		Path:        rel,
		IsDir:       prop.ResourceType.Collection != nil,
		ETag:        strings.Trim(prop.GetETag, `"`),
		FileID:      strings.TrimSpace(prop.FileID),
		ContentType: prop.GetContentType,
		Checksums:   strings.TrimSpace(prop.Checksums),
		IsEncrypted: prop.IsEncrypted == "1" || strings.EqualFold(prop.IsEncrypted, "true"),
		Permissions: strings.TrimSpace(prop.Permissions),
	}
	// Directories report their recursive size via oc:size; files use the
	// standard content length.
	if e.IsDir {
		e.Size, _ = strconv.ParseInt(strings.TrimSpace(prop.OCSize), 10, 64)
	} else {
		e.Size, _ = strconv.ParseInt(strings.TrimSpace(prop.GetContentLen), 10, 64)
	}
	if t, err := http.ParseTime(prop.GetLastModified); err == nil {
		e.LastModified = t
	}
	return e, true
}

// Stat returns the entry at remotePath (PROPFIND depth 0). The boolean is false
// when the path does not exist.
func (c *Client) Stat(ctx context.Context, remotePath string) (Entry, bool, error) {
	entries, err := c.PropFind(ctx, remotePath, 0)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return Entry{}, false, nil
		}
		return Entry{}, false, err
	}
	if len(entries) == 0 {
		return Entry{}, false, nil
	}
	return entries[0], true, nil
}

// Get opens the file at remotePath for reading. The caller must close the
// returned ReadCloser. The response headers (ETag, OC-Checksum) are returned
// for integrity verification by the transfer layer.
func (c *Client) Get(ctx context.Context, remotePath string) (io.ReadCloser, http.Header, error) {
	body, hdr, _, err := c.GetFrom(ctx, remotePath, 0)
	return body, hdr, err
}

// GetFrom opens the file at remotePath for reading starting at byte offset. When
// offset is 0 it performs a normal GET; otherwise it requests a Range. The
// returned status is 200 (full body — caller must restart from 0) or 206
// (partial — caller may append). The caller must close the body.
func (c *Client) GetFrom(ctx context.Context, remotePath string, offset int64) (io.ReadCloser, http.Header, int, error) {
	req, err := c.NewRequest(ctx, http.MethodGet, c.davURL(remotePath), nil)
	if err != nil {
		return nil, nil, 0, err
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, nil, 0, err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		defer resp.Body.Close()
		return nil, nil, resp.StatusCode, statusError("GET", remotePath, resp)
	}
	body := limitReadCloser(ctx, resp.Body, c.downLimiter)
	return body, resp.Header, resp.StatusCode, nil
}

// Put uploads data to remotePath with a single PUT. It is intended for small
// files; large files should use the chunked uploader (transfer package). The
// returned ETag identifies the stored revision.
func (c *Client) Put(ctx context.Context, remotePath string, body io.Reader, size int64) (string, error) {
	req, err := c.NewRequest(ctx, http.MethodPut, c.davURL(remotePath), body)
	if err != nil {
		return "", err
	}
	if size >= 0 {
		req.ContentLength = size
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := c.DoOnce(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		return "", statusError("PUT", remotePath, resp)
	}
	return strings.Trim(resp.Header.Get("ETag"), `"`), nil
}

// PutWithChecksum uploads data to remotePath with a single PUT, optionally
// passing an OC-Checksum (e.g. "SHA1:abc…") for the server to verify, and
// returns the stored revision's ETag and file ID. Intended for small files;
// large files use the chunked uploader.
func (c *Client) PutWithChecksum(ctx context.Context, remotePath string, body io.Reader, size int64, ocChecksum string) (etag, fileID string, err error) {
	req, err := c.NewRequest(ctx, http.MethodPut, c.davURL(remotePath), body)
	if err != nil {
		return "", "", err
	}
	if size >= 0 {
		req.ContentLength = size
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if ocChecksum != "" {
		req.Header.Set("OC-Checksum", ocChecksum)
	}
	resp, err := c.DoOnce(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		return "", "", statusError("PUT", remotePath, resp)
	}
	etag, fileID = revisionHeaders(resp.Header)
	return etag, fileID, nil
}

// revisionHeaders extracts the ETag and OC-FileId from a write response,
// preferring the OC-* variants Nextcloud sets on PUT/MOVE.
func revisionHeaders(h http.Header) (etag, fileID string) {
	etag = strings.Trim(h.Get("OC-ETag"), `"`)
	if etag == "" {
		etag = strings.Trim(h.Get("ETag"), `"`)
	}
	fileID = h.Get("OC-FileId")
	return etag, fileID
}

// Mkcol creates a directory (collection) at remotePath. An existing directory
// (405 Method Not Allowed) is treated as success.
func (c *Client) Mkcol(ctx context.Context, remotePath string) error {
	req, err := c.NewRequest(ctx, "MKCOL", c.davURL(remotePath), nil)
	if err != nil {
		return err
	}
	resp, err := c.DoOnce(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusMethodNotAllowed {
		return nil
	}
	return statusError("MKCOL", remotePath, resp)
}

// EnsureCollection creates the directory at remotePath and any missing parents,
// so callers can target a nested path that does not yet exist.
func (c *Client) EnsureCollection(ctx context.Context, remotePath string) error {
	remotePath = strings.Trim(remotePath, "/")
	if remotePath == "" {
		return nil
	}
	parts := strings.Split(remotePath, "/")
	cur := ""
	for _, p := range parts {
		if cur == "" {
			cur = p
		} else {
			cur += "/" + p
		}
		if err := c.Mkcol(ctx, cur); err != nil {
			return err
		}
	}
	return nil
}

// Delete removes the file or directory at remotePath.
func (c *Client) Delete(ctx context.Context, remotePath string) error {
	req, err := c.NewRequest(ctx, http.MethodDelete, c.davURL(remotePath), nil)
	if err != nil {
		return err
	}
	resp, err := c.DoOnce(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return statusError("DELETE", remotePath, resp)
	}
	return nil
}

// Move renames/moves src to dst (both files-root-relative paths).
func (c *Client) Move(ctx context.Context, src, dst string) error {
	req, err := c.NewRequest(ctx, "MOVE", c.davURL(src), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Destination", c.davURL(dst))
	req.Header.Set("Overwrite", "T")
	resp, err := c.DoOnce(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		return statusError("MOVE", src, resp)
	}
	return nil
}

// statusError builds a descriptive error from an unexpected response, including
// a short snippet of the body.
func statusError(op, path string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
	snippet := strings.TrimSpace(string(body))
	if len(snippet) > 300 {
		snippet = snippet[:300] + "…"
	}
	return fmt.Errorf("%s %q: server returned %s: %s", op, path, resp.Status, snippet)
}
