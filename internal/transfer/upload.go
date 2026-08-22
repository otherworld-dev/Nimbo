package transfer

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/otherworld/nimbo/internal/transport"
)

// Chunk sizing (vars so tests can exercise the chunked path with small files).
var (
	// chunkThreshold is the size above which uploads use the chunked API.
	chunkThreshold int64 = 10 << 20 // 10 MiB
	// minChunkSize is the v2 minimum (except the final chunk).
	minChunkSize int64 = 10 << 20 // 10 MiB (>= the 5 MiB protocol minimum)
)

// maxChunks is the v2 cap on chunk count; chunk size scales to stay under it.
const maxChunks = 10000

// Per-operation retry/poll knobs (vars so tests can shrink the waits).
var (
	chunkAttempts    = 4 // attempts per chunk (transient failures only)
	assembleAttempts = 3 // attempts for the final MOVE
	assemblePollGap  = 15 * time.Second
	assemblePollMin  = 2 * time.Minute
	// assemblePollBudget is how long to keep checking the destination for an
	// assembly whose MOVE response was lost. Server-side concatenation of a
	// huge file takes real time (hundreds of GB at disk speeds = tens of
	// minutes), and giving up early turns a SUCCEEDED upload into a failure —
	// or worse, a re-MOVE against the consumed session.
	assemblePollBudget = func(size int64) time.Duration {
		d := time.Duration(size/(100<<20)) * time.Second // ~100 MB/s server-side
		if d < assemblePollMin {
			d = assemblePollMin
		}
		if d > 45*time.Minute {
			d = 45 * time.Minute
		}
		return d
	}
	// uploadIDSalt makes chunk-session IDs per-machine: two clients sharing an
	// account must never interleave chunks in one server-side session. The
	// hostname is stable across restarts (per-machine resume keeps working).
	uploadIDSalt = defaultUploadIDSalt()
)

func defaultUploadIDSalt() string {
	h, err := os.Hostname()
	if err != nil {
		return "nimbo"
	}
	return h
}

// Upload sends localPath to remotePath. Small files go via a single PUT; large
// files use the resumable chunked v2 API. In both cases the content SHA1 is sent
// as OC-Checksum for the server to verify. The returned FileResult carries the
// new ETag/FileID and the local file's size and mtime for the baseline.
func Upload(ctx context.Context, c *transport.Client, localPath, remotePath string) (FileResult, error) {
	return UploadProgress(ctx, c, localPath, remotePath, nil)
}

// UploadProgress is Upload with an optional progress callback invoked with the
// number of bytes sent as they upload. The callback can see small negative
// deltas: bytes reported for a chunk attempt that failed are withdrawn before
// the chunk restarts, so the running total stays honest.
func UploadProgress(ctx context.Context, c *transport.Client, localPath, remotePath string, prog func(int64)) (FileResult, error) {
	fi, err := os.Stat(localPath)
	if err != nil {
		return FileResult{}, err
	}
	sum, err := sha1File(localPath)
	if err != nil {
		return FileResult{}, err
	}
	// The hash pass takes minutes on a huge file. If the file changed under it
	// (still being copied in, an app mid-save), the hash doesn't match the
	// bytes any longer — refuse now rather than upload torn content; the
	// caller retries once the writer has settled.
	if err := statUnchanged(localPath, fi); err != nil {
		return FileResult{}, err
	}
	checksum := ocChecksum(sum)

	var etag, fileID string
	chunked := fi.Size() > chunkThreshold
	if !chunked {
		etag, fileID, err = uploadSingle(ctx, c, localPath, remotePath, fi.Size(), checksum, prog)
	} else {
		etag, fileID, err = uploadChunked(ctx, c, localPath, remotePath, fi.Size(), fi.ModTime().UnixNano(), checksum, prog)
	}
	if err != nil {
		return FileResult{}, err
	}

	// One stat serves two jobs: (a) some server/storage configurations omit
	// the revision headers on write, so fetch ETag/FileID when missing; (b)
	// for a chunked upload, compare the server's stored checksum against ours
	// when it reports one — the last line of defence on servers that do NOT
	// verify OC-Checksum at assembly time, where a resumed session's stale
	// chunks would otherwise land silently as corrupt content.
	if chunked || etag == "" || fileID == "" {
		if e, ok, serr := c.Stat(ctx, remotePath); serr == nil && ok {
			if chunked {
				if s := e.ContentSHA1(); s != "" && !strings.EqualFold(s, sum) {
					return FileResult{}, fmt.Errorf("%s: server checksum %s does not match uploaded content %s", filepath.Base(remotePath), s, sum)
				}
			}
			if etag == "" {
				etag = e.ETag
			}
			if fileID == "" {
				fileID = e.FileID
			}
		}
	}

	return FileResult{
		ETag:        etag,
		FileID:      fileID,
		Size:        fi.Size(),
		MTimeNanos:  fi.ModTime().UnixNano(),
		ContentSHA1: sum,
	}, nil
}

// statUnchanged verifies path still has the size and mtime captured in fi.
func statUnchanged(path string, fi os.FileInfo) error {
	cur, err := os.Stat(path)
	if err != nil {
		return err
	}
	if cur.Size() != fi.Size() || !cur.ModTime().Equal(fi.ModTime()) {
		return fmt.Errorf("%s changed while it was being read", filepath.Base(path))
	}
	return nil
}

// uploadSingle performs a one-shot PUT, riding out transient failures.
func uploadSingle(ctx context.Context, c *transport.Client, localPath, remotePath string, size int64, checksum string, prog func(int64)) (etag, fileID string, err error) {
	// Each attempt gets a FRESH file handle: net/http reads request bodies on
	// its own goroutine, and a shared handle's seek offset would race a
	// replayed body against a straggling reader from the failed attempt.
	var cur *os.File
	defer func() {
		if cur != nil {
			cur.Close()
		}
	}()
	var sent atomic.Int64
	newBody := func() (io.Reader, error) {
		f, oerr := os.Open(localPath)
		if oerr != nil {
			return nil, oerr
		}
		if cur != nil {
			cur.Close()
		}
		cur = f
		if s := sent.Swap(0); s > 0 && prog != nil {
			prog(-s)
		}
		// Never hand out f itself: it satisfies io.ReadCloser, and the HTTP
		// client closes request bodies after each attempt — later reads from
		// the retry path would then hit a closed file.
		var r io.Reader = io.NopCloser(f)
		if prog != nil {
			r = &progReader{r: f, fn: func(d int64) { sent.Add(d); prog(d) }}
		}
		return r, nil
	}
	for attempt := 0; attempt < chunkAttempts; attempt++ {
		if attempt > 0 {
			if !transport.Retryable(err) {
				return "", "", err
			}
			if serr := sleepBackoff(ctx, attempt); serr != nil {
				return "", "", err
			}
		}
		etag, fileID, err = c.PutWithChecksum(ctx, remotePath, newBody, size, checksum)
		if err == nil || ctx.Err() != nil {
			return etag, fileID, err
		}
	}
	return "", "", err
}

// uploadChunked uploads in numbered chunks then assembles.
//
// The session ID is derived from the file's identity (path + size + mtime), so
// a retry of the same unchanged file — minutes or days later, across restarts —
// finds its previous session and skips the chunks already uploaded. That is
// what makes a 300GB upload survivable on a connection that occasionally dies:
// nothing short of the file itself changing restarts it from byte zero.
func uploadChunked(ctx context.Context, c *transport.Client, localPath, remotePath string, size, mtimeNanos int64, checksum string, prog func(int64)) (etag, fileID string, err error) {
	uploadID := uploadIDFor(remotePath, size, mtimeNanos)
	if err := c.CreateUpload(ctx, uploadID); err != nil {
		return "", "", err
	}
	// Deliberately NO cleanup on failure: the session and its chunks stay on
	// the server so the next attempt resumes (the server GCs stale sessions,
	// and a successful assembly consumes it). Only a poisoned session —
	// assembly refused outright — is deleted, below.

	chunkSize := chunkSizeFor(size)
	existing, err := c.ListChunks(ctx, uploadID)
	if err != nil {
		return "", "", err
	}

	f, err := os.Open(localPath)
	if err != nil {
		return "", "", err
	}
	defer f.Close()

	var offset int64
	last := 0
	for i := 1; offset < size; i++ {
		last = i
		n := chunkSize
		if remaining := size - offset; remaining < n {
			n = remaining
		}
		name := fmt.Sprintf("%05d", i)
		if got, ok := existing[name]; !ok || got != n {
			if err := putChunkRetry(ctx, c, uploadID, name, f, offset, n, remotePath, prog); err != nil {
				return "", "", err
			}
		} else if prog != nil {
			prog(n) // already-uploaded chunk counts toward progress
		}
		offset += n
	}

	// A resumed session written under a different chunk layout (older app
	// version, different size calculation) can hold chunks NUMBERED BEYOND
	// today's final one — the server concatenates every member, so a stale
	// tail would corrupt the assembled file. Prune them before assembling.
	for name := range existing {
		if idx, perr := strconv.Atoi(name); perr != nil || idx > last {
			if derr := c.DeleteChunk(ctx, uploadID, name); derr != nil {
				return "", "", fmt.Errorf("could not remove stale chunk %s: %w", name, derr)
			}
		}
	}

	return assembleRetry(ctx, c, uploadID, remotePath, size, checksum)
}

// putChunkRetry uploads one chunk, retrying transient failures with backoff.
// Bytes reported for a failed attempt are withdrawn (negative delta) before the
// chunk restarts.
func putChunkRetry(ctx context.Context, c *transport.Client, uploadID, name string, f *os.File, offset, n int64, destPath string, prog func(int64)) error {
	var sent atomic.Int64
	newBody := func() (io.Reader, error) {
		if s := sent.Swap(0); s > 0 && prog != nil {
			prog(-s)
		}
		var r io.Reader = io.NewSectionReader(f, offset, n)
		if prog != nil {
			r = &progReader{r: r, fn: func(d int64) { sent.Add(d); prog(d) }}
		}
		return r, nil
	}
	var err error
	for attempt := 0; attempt < chunkAttempts; attempt++ {
		if attempt > 0 {
			if !transport.Retryable(err) {
				return err
			}
			if serr := sleepBackoff(ctx, attempt); serr != nil {
				return err
			}
		}
		if err = c.PutChunk(ctx, uploadID, name, newBody, n, destPath); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return err
		}
	}
	return err
}

// assembleRetry finalises the session with the MOVE of ".file", riding out
// transient failures. On a huge file the server can spend many minutes
// assembling and a proxy may cut the response while assembly carries on and
// lands — so after a failure that COULD mean an in-flight/landed assembly
// (transient errors, 404/409 on a consumed session, timeouts) the destination
// is polled, on a budget scaled to the file size, before re-MOVEing.
//
// The session is deleted ONLY when the server judged the assembled content
// itself bad (400/422 — checksum mismatch, malformed): those chunks are
// poison, and the deterministic session ID would reuse them forever. Every
// other failure — a 423 lock on the destination, quota, auth blips, a WAF
// tantrum — keeps the session intact so the caller's retry RESUMES instead of
// re-uploading hundreds of gigabytes.
func assembleRetry(ctx context.Context, c *transport.Client, uploadID, remotePath string, size int64, checksum string) (etag, fileID string, err error) {
	// The pre-assembly baseline: without a KNOWN prior state of the
	// destination, polling can mistake an old same-size file for our upload —
	// so if this Stat fails, destination polling is disabled entirely.
	preETag, preOK := "", false
	if e, ok, serr := c.Stat(ctx, remotePath); serr == nil {
		preOK = true
		if ok {
			preETag = e.ETag
		}
	}
	for attempt := 0; attempt < assembleAttempts; attempt++ {
		if attempt > 0 {
			if serr := sleepBackoff(ctx, attempt); serr != nil {
				return "", "", err
			}
		}
		etag, fileID, err = c.AssembleUpload(ctx, uploadID, remotePath, size, checksum)
		if err == nil {
			return etag, fileID, nil
		}
		if ctx.Err() != nil {
			return "", "", err
		}
		code := transport.StatusCode(err)
		if code == 400 || code == 422 {
			_ = c.DeleteUpload(ctx, uploadID) // poisoned content — start clean next time
			return "", "", err
		}
		mayHaveLanded := transport.Retryable(err) || code == 404 || code == 409
		if mayHaveLanded && preOK {
			if e, ok := waitForAssembled(ctx, c, remotePath, size, preETag); ok {
				return e.ETag, e.FileID, nil
			}
		}
		if !transport.Retryable(err) {
			return "", "", err // session kept; the caller's backoff retry resumes
		}
	}
	return "", "", err
}

// waitForAssembled polls remotePath for the assembled upload landing after a
// lost MOVE response. It demands both the exact size and an ETag different
// from the pre-assembly one — a same-size older version must not read as ours.
func waitForAssembled(ctx context.Context, c *transport.Client, remotePath string, size int64, preETag string) (transport.Entry, bool) {
	deadline := time.Now().Add(assemblePollBudget(size))
	for {
		if e, ok, err := c.Stat(ctx, remotePath); err == nil && ok && e.Size == size && e.ETag != preETag {
			return e, true
		}
		if time.Now().After(deadline) {
			return transport.Entry{}, false
		}
		if err := sleepCtx(ctx, assemblePollGap); err != nil {
			return transport.Entry{}, false
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// chunkSizeFor picks a chunk size that respects the protocol minimum while
// keeping the chunk count under the cap for very large files.
func chunkSizeFor(size int64) int64 {
	cs := minChunkSize
	if needed := (size + maxChunks - 1) / maxChunks; needed > cs {
		cs = needed
	}
	return cs
}

// uploadIDFor derives the chunked-upload session ID from the file's identity.
// Stable while the file is unchanged (that's what enables resume); any change
// to path, size or mtime yields a fresh session so stale chunks are never
// mixed into a different version. (If content changed under identical
// size+mtime, the OC-Checksum check refuses the assembly and the poisoned
// session is deleted — self-healing.)
func uploadIDFor(remotePath string, size, mtimeNanos int64) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s|%s|%d|%d", uploadIDSalt, remotePath, size, mtimeNanos)
	return "nimbo-" + hex.EncodeToString(h.Sum(nil))
}
