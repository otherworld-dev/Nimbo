package transfer

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

// testHookBeforeSend runs after the file is hashed and checked, just before
// its bytes are sent: the window a program writing to the file can land in.
var testHookBeforeSend func()

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
	// A file caught changing under an earlier upload is not read again while a
	// program still has it open to write (Outlook and an attached .pst): each
	// try would send the whole file only to find it torn again (Deck #691).
	if err := UploadDeferred(localPath); err != nil {
		return FileResult{}, err
	}
	res, err := uploadOnce(ctx, c, localPath, remotePath, prog)
	var changed *ChangedError
	switch {
	case errors.As(err, &changed) && changed.InPlace:
		markBusyWriter(localPath)
	case err == nil:
		clearBusyWriter(localPath)
	}
	return res, err
}

func uploadOnce(ctx context.Context, c *transport.Client, localPath, remotePath string, prog func(int64)) (FileResult, error) {
	fi, err := statShared(localPath)
	if err != nil {
		return FileResult{}, err
	}
	// The checksum declared must be the checksum of the bytes that arrive.
	// Hashing the file and then reading it again to send it let a program
	// writing in between (Outlook, again) leave the server a torn copy under a
	// checksum it did not match (Deck #691). So a small file is read once and
	// those exact bytes are hashed and sent; a large one is hashed again as it
	// is sent, and not assembled unless the two agree.
	chunked := fi.Size() > chunkThreshold
	var data []byte
	var sum string
	if chunked {
		sum, err = sha1File(localPath)
	} else if data, err = readShared(localPath); err == nil {
		h := sha1.Sum(data)
		sum = hex.EncodeToString(h[:])
	}
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
	if !chunked && int64(len(data)) != fi.Size() {
		return FileResult{}, &ChangedError{Path: localPath, InPlace: true}
	}
	checksum := ocChecksum(sum)
	if testHookBeforeSend != nil {
		testHookBeforeSend()
	}

	var etag, fileID string
	if !chunked {
		etag, fileID, err = uploadSingle(ctx, c, data, remotePath, checksum, fi.ModTime(), prog)
	} else {
		etag, fileID, err = uploadChunked(ctx, c, localPath, remotePath, fi, sum, prog)
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

// statUnchanged verifies path is still the file captured in fi, with the same
// size and mtime. fi must come from statShared, so its identity is fixed.
func statUnchanged(path string, fi os.FileInfo) error {
	cur, err := statShared(path)
	if err != nil {
		return err
	}
	if !os.SameFile(fi, cur) {
		return &ChangedError{Path: path} // replaced: an editor's atomic save
	}
	if cur.Size() != fi.Size() || !cur.ModTime().Equal(fi.ModTime()) {
		return &ChangedError{Path: path, InPlace: true}
	}
	return nil
}

// readShared reads a whole file through openShared.
func readShared(path string) ([]byte, error) {
	f, err := openShared(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// uploadSingle performs a one-shot PUT of data, riding out transient failures.
// The bytes come from memory, so every attempt sends exactly what was hashed.
func uploadSingle(ctx context.Context, c *transport.Client, data []byte, remotePath, checksum string, mtime time.Time, prog func(int64)) (etag, fileID string, err error) {
	var sent atomic.Int64
	newBody := func() (io.Reader, error) {
		if s := sent.Swap(0); s > 0 && prog != nil {
			prog(-s)
		}
		var r io.Reader = bytes.NewReader(data)
		if prog != nil {
			r = &progReader{r: r, fn: func(d int64) { sent.Add(d); prog(d) }}
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
		etag, fileID, err = c.PutWithChecksum(ctx, remotePath, newBody, int64(len(data)), checksum, mtime)
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
func uploadChunked(ctx context.Context, c *transport.Client, localPath, remotePath string, orig os.FileInfo, sum string, prog func(int64)) (etag, fileID string, err error) {
	size := orig.Size()
	uploadID := uploadIDFor(remotePath, size, orig.ModTime().UnixNano())
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

	f, err := openShared(localPath)
	if err != nil {
		return "", "", err
	}
	defer f.Close()

	// Hash what is actually sent, and for chunks a resumed session already
	// holds, what the file holds there now: the file is only assembled if that
	// is the file that was hashed (Deck #691).
	var running hash.Hash = sha1.New()
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
			if running, err = putChunkRetry(ctx, c, uploadID, name, f, offset, n, remotePath, prog, running); err != nil {
				return "", "", err
			}
		} else {
			if _, err := io.Copy(running, io.NewSectionReader(f, offset, n)); err != nil {
				return "", "", err
			}
			if prog != nil {
				prog(n) // already-uploaded chunk counts toward progress
			}
		}
		offset += n
	}
	if got := hex.EncodeToString(running.Sum(nil)); !strings.EqualFold(got, sum) {
		// Torn: the file moved while it was being sent. Drop the chunks so a
		// later resume can't assemble them either.
		_ = c.DeleteUpload(ctx, uploadID)
		// Written into (the handle is the file that was hashed), or saved over
		// (a new file had replaced it by the time it was opened to send)?
		inPlace := false
		if sent, serr := f.Stat(); serr == nil {
			inPlace = os.SameFile(orig, sent)
		}
		return "", "", &ChangedError{Path: localPath, InPlace: inPlace}
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

	return assembleRetry(ctx, c, uploadID, remotePath, size, ocChecksum(sum), orig.ModTime())
}

// putChunkRetry uploads one chunk, retrying transient failures with backoff.
// Bytes reported for a failed attempt are withdrawn (negative delta) before the
// chunk restarts. running is the hash of everything before this chunk; the hash
// including this chunk's bytes, as the successful attempt read them, comes
// back. Each attempt hashes into its own copy, so an abandoned attempt's body
// still being read can't disturb the one that counts.
func putChunkRetry(ctx context.Context, c *transport.Client, uploadID, name string, f *os.File, offset, n int64, destPath string, prog func(int64), running hash.Hash) (hash.Hash, error) {
	before, err := running.(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		return nil, err
	}
	var sent atomic.Int64
	var mu sync.Mutex
	var latest *lockedHash
	newBody := func() (io.Reader, error) {
		if s := sent.Swap(0); s > 0 && prog != nil {
			prog(-s)
		}
		h := sha1.New()
		if err := h.(encoding.BinaryUnmarshaler).UnmarshalBinary(before); err != nil {
			return nil, err
		}
		lh := &lockedHash{h: h}
		mu.Lock()
		latest = lh
		mu.Unlock()
		var r io.Reader = io.TeeReader(io.NewSectionReader(f, offset, n), lh)
		if prog != nil {
			r = &progReader{r: r, fn: func(d int64) { sent.Add(d); prog(d) }}
		}
		return r, nil
	}
	for attempt := 0; attempt < chunkAttempts; attempt++ {
		if attempt > 0 {
			if !transport.Retryable(err) {
				return nil, err
			}
			if serr := sleepBackoff(ctx, attempt); serr != nil {
				return nil, err
			}
		}
		if err = c.PutChunk(ctx, uploadID, name, newBody, n, destPath); err == nil {
			mu.Lock()
			lh := latest
			mu.Unlock()
			if lh == nil {
				return nil, fmt.Errorf("chunk %s was sent without reading its body", name)
			}
			lh.mu.Lock()
			defer lh.mu.Unlock()
			return lh.h, nil
		}
		if ctx.Err() != nil {
			return nil, err
		}
	}
	return nil, err
}

// lockedHash is a hash safe to write from the HTTP client's body reader while
// its result is read here.
type lockedHash struct {
	mu sync.Mutex
	h  hash.Hash
}

func (l *lockedHash) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.h.Write(p)
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
func assembleRetry(ctx context.Context, c *transport.Client, uploadID, remotePath string, size int64, checksum string, mtime time.Time) (etag, fileID string, err error) {
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
		etag, fileID, err = c.AssembleUpload(ctx, uploadID, remotePath, size, checksum, mtime)
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
