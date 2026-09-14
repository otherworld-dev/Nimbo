package transport

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
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
	Checksums    string    // raw oc:checksums value, e.g. "SHA1:abc MD5:def"
	IsFavorite   bool      // oc:favorite — the user starred it; false also when the server does not report the property
	IsEncrypted  bool      // nc:is-encrypted — an end-to-end encrypted folder (contents are opaque to clients without E2EE keys)
	Permissions  string    // oc:permissions, e.g. "RGDNVW" (file) / "RMGCK" (dir); empty = unknown
	Lock         *LockInfo // files_lock state; nil = unlocked OR the server didn't say (see LockInfo)
}

// LockOwnerType identifies who took a lock, from nc:lock-owner-type.
type LockOwnerType int

const (
	LockOwnerUser  LockOwnerType = 0 // a person locked it by hand, e.g. in the web UI
	LockOwnerApp   LockOwnerType = 1 // a collaborative editor — Nextcloud Office or Text
	LockOwnerToken LockOwnerType = 2 // a WebDAV client holding a lock token
)

// LockInfo is the files_lock state of an entry.
//
// A nil *LockInfo means "not locked, or the server did not tell us" — the two are
// deliberately indistinguishable here so that a server without the files_lock app
// can never be read as "locked". Ask Capabilities whether locking is supported at
// all before telling a user that nobody has a file open.
type LockInfo struct {
	Owner        string        // nc:lock-owner — the login name; EMPTY for an app lock
	OwnerDisplay string        // nc:lock-owner-displayname — a person for type 0, the APP's name for type 1
	OwnerEditor  string        // nc:lock-owner-editor — the app id holding a type-1 lock, e.g. "text"
	OwnerType    LockOwnerType // nc:lock-owner-type
	Token        string        // nc:lock-token
	Since        time.Time     // nc:lock-time (epoch seconds)
	Timeout      time.Duration // nc:lock-timeout; zero means the server set no expiry
}

// AppName is the editor holding an app lock, as a human would say it. Empty for
// a lock that belongs to a person rather than an app.
//
// Type-1 locks carry no nc:lock-owner at all — verified against a live Text
// session — so OwnerDisplay is the APP's name ("Text"), not somebody's. Treating
// it as a person produces "Text is editing this", which reads like a colleague
// called Text.
func (l *LockInfo) AppName() string {
	if l == nil || l.OwnerType != LockOwnerApp {
		return ""
	}
	if l.OwnerDisplay != "" {
		return l.OwnerDisplay
	}
	if l.OwnerEditor != "" {
		return l.OwnerEditor
	}
	return "another app"
}

// ContentSHA1 is the server's SHA1 for this entry, parsed out of the raw
// oc:checksums value, or "" when the server did not provide one.
func (e Entry) ContentSHA1() string {
	for _, tok := range strings.Fields(e.Checksums) {
		if strings.HasPrefix(strings.ToUpper(tok), "SHA1:") {
			return strings.ToLower(tok[len("SHA1:"):])
		}
	}
	return ""
}

// HeldByOther reports whether this lock belongs to somebody other than the
// signed-in user. An unattributable lock counts as someone else's: the file is
// locked and we cannot prove it is ours, so the safe answer is to say so. A nil
// receiver is simply not locked, which saves every caller a nil check.
func (l *LockInfo) HeldByOther(login string) bool {
	if l == nil {
		return false
	}
	return login == "" || !strings.EqualFold(l.Owner, login)
}

// LockedByOther is HeldByOther for an entry straight off a listing.
func (e Entry) LockedByOther(login string) bool { return e.Lock.HeldByOther(login) }

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

// davRel maps an href from a server response back to a files-root-relative
// path. The href is LOCATED against davBase rather than trimmed of it: a
// Nextcloud installed at a URL subpath answers with
// "/nextcloud/remote.php/dav/files/alice/x", and a plain TrimPrefix silently
// left every such path untouched — re-prepended on the next request, that
// 404s, so nothing synced against such a server at all (GitHub #3/#4). An href
// with no files root for this user in it is an error, not a path: passing it
// through is how a misread listing became planned deletes, and skipping it
// would read as "deleted on the server".
func (c *Client) davRel(href string) (string, error) {
	p, err := unescapeHref(href)
	if err != nil {
		return "", fmt.Errorf("href %q: %w", href, err)
	}
	base := c.davBase()
	i := strings.Index(p, base)
	if i < 0 {
		return "", fmt.Errorf("href %q is outside %s", p, base)
	}
	rest := p[i+len(base):]
	if rest != "" && rest[0] != '/' {
		return "", fmt.Errorf("href %q is outside %s", p, base) // ".../alice2/x" is not alice's
	}
	return strings.Trim(rest, "/"), nil
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

// entryProps is the property list every Entry is built from. Listing and the
// favourites REPORT share it deliberately: they return the same type, a client
// renders both with the same row code, and a shorter list would show files of
// size 0 with no type and no date rather than fail visibly.
const entryProps = `    <d:getetag/>
    <d:getlastmodified/>
    <d:getcontentlength/>
    <d:getcontenttype/>
    <d:resourcetype/>
    <oc:fileid/>
    <oc:size/>
    <oc:checksums/>
    <oc:permissions/>
    <oc:favorite/>
    <nc:is-encrypted/>
    <nc:lock/>
    <nc:lock-owner/>
    <nc:lock-owner-displayname/>
    <nc:lock-owner-editor/>
    <nc:lock-owner-type/>
    <nc:lock-time/>
    <nc:lock-timeout/>
    <nc:lock-token/>
`

// propfindBody requests exactly the properties Entry exposes.
const propfindBody = `<?xml version="1.0" encoding="UTF-8"?>
<d:propfind xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns" xmlns:nc="http://nextcloud.org/ns">
  <d:prop>
` + entryProps + `  </d:prop>
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
	FileID string `xml:"fileid"`
	// oc:id — the file id zero-padded to 8 digits followed by the server's
	// instance id. Unlike fileid it changes when the INSTANCE changes, which is
	// what the local-route same-server check needs. Only rootIDBody asks for it.
	ID          string `xml:"id"`
	OCSize      string `xml:"size"`
	IsEncrypted string `xml:"is-encrypted"`
	Permissions string `xml:"permissions"`
	Favorite    string `xml:"favorite"`
	// oc:checksums wraps one or more <oc:checksum> children; the digest text is
	// in the child, not the wrapper.
	Checksums string `xml:"checksums>checksum"`
	// files_lock properties (nc namespace). Absent unless the app is installed —
	// the server then returns them in a 404 propstat, which parseResponse skips.
	Lock             string `xml:"lock"`
	LockOwner        string `xml:"lock-owner"`
	LockOwnerDisplay string `xml:"lock-owner-displayname"`
	LockOwnerEditor  string `xml:"lock-owner-editor"`
	LockOwnerType    string `xml:"lock-owner-type"`
	LockTime         string `xml:"lock-time"`
	LockTimeout      string `xml:"lock-timeout"`
	LockToken        string `xml:"lock-token"`
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

	entries := make([]Entry, 0, len(ms.Responses))
	for _, r := range ms.Responses {
		e, ok, err := c.parseResponse(r)
		if err != nil {
			return nil, fmt.Errorf("PROPFIND %q: %w", remotePath, err)
		}
		if ok {
			entries = append(entries, e)
		}
	}
	return entries, nil
}

// parseResponse converts a single PROPFIND <response> into an Entry, selecting
// the 200-status propstat block. ok is false if the row should be skipped; err
// is set when the row's href cannot be placed under this user's files root
// (see davRel), which invalidates the whole listing.
func (c *Client) parseResponse(r davResponse) (Entry, bool, error) {
	rel, err := c.davRel(r.Href)
	if err != nil {
		return Entry{}, false, err
	}

	var prop *davProp
	for i := range r.Propstat {
		if strings.Contains(r.Propstat[i].Status, "200") {
			prop = &r.Propstat[i].Prop
			break
		}
	}
	if prop == nil {
		return Entry{}, false, nil
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
		IsFavorite:  prop.Favorite == "1" || strings.EqualFold(prop.Favorite, "true"),
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
	// files_lock. Only a positive nc:lock yields a record: an unlocked file
	// reports it as an EMPTY string, and a server without the app omits it
	// entirely (it arrives in a 404 propstat we never read). Both mean "not
	// locked" — we never claim a file is locked on thin evidence.
	if prop.Lock == "1" || strings.EqualFold(prop.Lock, "true") {
		li := &LockInfo{
			Owner:        strings.TrimSpace(prop.LockOwner),
			OwnerDisplay: strings.TrimSpace(prop.LockOwnerDisplay),
			OwnerEditor:  strings.TrimSpace(prop.LockOwnerEditor),
			Token:        strings.TrimSpace(prop.LockToken),
		}
		if n, err := strconv.Atoi(strings.TrimSpace(prop.LockOwnerType)); err == nil {
			li.OwnerType = LockOwnerType(n)
		}
		if secs, err := strconv.ParseInt(strings.TrimSpace(prop.LockTime), 10, 64); err == nil && secs > 0 {
			li.Since = time.Unix(secs, 0)
		}
		// nc:lock-timeout is NOT reliably "seconds remaining" — a live NC 34.0.2
		// returned -60 for a one-second-old lock. Non-positive is read as "the
		// server set no expiry", which is the truth on a default install:
		// files_lock does not expire locks unless an admin configures it.
		if secs, err := strconv.Atoi(strings.TrimSpace(prop.LockTimeout)); err == nil && secs > 0 {
			li.Timeout = time.Duration(secs) * time.Second
		}
		e.Lock = li
	}
	return e, true, nil
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
//
// newBody must return a fresh reader of the whole content each call — it backs
// the request's GetBody so the transport can replay the PUT after a
// connection-level failure (HTTP/2 GOAWAY, stale reused connection).
func (c *Client) PutWithChecksum(ctx context.Context, remotePath string, newBody func() (io.Reader, error), size int64, ocChecksum string) (etag, fileID string, err error) {
	body, err := newBody()
	if err != nil {
		return "", "", err
	}
	req, err := c.NewRequest(ctx, http.MethodPut, c.davURL(remotePath), body)
	if err != nil {
		return "", "", err
	}
	req.GetBody = func() (io.ReadCloser, error) {
		r, err := newBody()
		if err != nil {
			return nil, err
		}
		return io.NopCloser(r), nil
	}
	// Hashed, not the raw path: header values must stay printable ASCII, and
	// a path can contain anything.
	pathHash := sha1.Sum([]byte(remotePath))
	req.Header.Set("Idempotency-Key", "nimbo-put-"+hex.EncodeToString(pathHash[:8]))
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

// LockResult is what the server returns from a LOCK: the token identifying the
// lock, the file's NEW ETag (locking bumps it), and the lock's own state. One
// LOCK therefore tells the client everything — no follow-up PROPFIND.
type LockResult struct {
	Token string
	ETag  string
	Info  LockInfo
}

// lockProp is a LOCK/UNLOCK reply body: a bare <d:prop>, NOT a multistatus. It
// reuses davProp so the lock fields are parsed by exactly one set of tags.
type lockProp struct {
	XMLName xml.Name `xml:"prop"`
	davProp
}

func parseLockResult(body []byte) (LockResult, error) {
	var p lockProp
	if err := xml.Unmarshal(body, &p); err != nil {
		return LockResult{}, fmt.Errorf("decode LOCK response: %w", err)
	}
	res := LockResult{
		Token: strings.TrimSpace(p.LockToken),
		ETag:  strings.Trim(strings.TrimSpace(p.GetETag), `"`),
		Info: LockInfo{
			Owner:        strings.TrimSpace(p.LockOwner),
			OwnerDisplay: strings.TrimSpace(p.LockOwnerDisplay),
			OwnerEditor:  strings.TrimSpace(p.LockOwnerEditor),
			Token:        strings.TrimSpace(p.LockToken),
		},
	}
	if n, err := strconv.Atoi(strings.TrimSpace(p.LockOwnerType)); err == nil {
		res.Info.OwnerType = LockOwnerType(n)
	}
	return res, nil
}

// Lock takes a files_lock lock on remotePath, via the app's user-lock flow
// (X-User-Lock) rather than the native WebDAV token flow.
//
// Re-locking a lock we already hold returns 200 with the SAME token — that is
// how the heartbeat works; there is no separate refresh verb. A lock held by
// somebody else returns 423, which IsLocked recognises.
func (c *Client) Lock(ctx context.Context, remotePath string) (LockResult, error) {
	req, err := c.NewRequest(ctx, "LOCK", c.davURL(remotePath), nil)
	if err != nil {
		return LockResult{}, err
	}
	req.Header.Set("X-User-Lock", "1")
	resp, err := c.DoOnce(req)
	if err != nil {
		return LockResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return LockResult{}, statusError("LOCK", remotePath, resp)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return LockResult{}, err
	}
	return parseLockResult(body)
}

// Unlock releases a lock WE hold.
//
// Only ever call this for a lock in our own registry: releasing somebody else's
// returns 423 and is not ours to clear. A second UNLOCK of the same path returns
// 412 Precondition Failed — NOT 404 — which simply means the lock is already
// gone, so it counts as success. The startup sweep hits that routinely, and
// treating it as an error would log a failure on every boot.
func (c *Client) Unlock(ctx context.Context, remotePath string) error {
	req, err := c.NewRequest(ctx, "UNLOCK", c.davURL(remotePath), nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-User-Lock", "1")
	resp, err := c.DoOnce(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent, http.StatusPreconditionFailed:
		return nil
	}
	return statusError("UNLOCK", remotePath, resp)
}

// statusError builds a descriptive error from an unexpected response, including
// a short snippet of the body.
// IsLocked reports whether err is a 423 Locked from the server — files_lock
// refusing a write because somebody else holds the file.
//
// This package returns untyped errors with the status formatted into the
// message, so a substring match is the only option; it keys on the exact phrase
// statusError builds rather than a bare "423", which would also match a path or
// a byte count. One definition, because both the agent (for the user-facing
// message) and the transfer executor (which must not retry a lock) need it.
func IsLocked(err error) bool {
	return StatusCode(err) == http.StatusLocked ||
		(err != nil && strings.Contains(err.Error(), "server returned 423"))
}

func statusError(op, path string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
	snippet := strings.TrimSpace(string(body))
	if len(snippet) > 300 {
		snippet = snippet[:300] + "…"
	}
	return &StatusError{Op: op, Path: path, Code: resp.StatusCode, Status: resp.Status, Snippet: snippet}
}
