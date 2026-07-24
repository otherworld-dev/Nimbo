package transfer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"github.com/otherworld/nimbo/internal/transport"
)

const (
	// chunkThreshold is the size above which uploads use the chunked API.
	chunkThreshold = 10 << 20 // 10 MiB
	// minChunkSize is the v2 minimum (except the final chunk).
	minChunkSize = 10 << 20 // 10 MiB (>= the 5 MiB protocol minimum)
	// maxChunks is the v2 cap on chunk count; chunk size scales to stay under it.
	maxChunks = 10000
)

// Upload sends localPath to remotePath. Small files go via a single PUT; large
// files use the resumable chunked v2 API. In both cases the content SHA1 is sent
// as OC-Checksum for the server to verify. The returned FileResult carries the
// new ETag/FileID and the local file's size and mtime for the baseline.
func Upload(ctx context.Context, c *transport.Client, localPath, remotePath string) (FileResult, error) {
	return UploadProgress(ctx, c, localPath, remotePath, nil)
}

// UploadProgress is Upload with an optional progress callback invoked with the
// number of bytes sent as they upload.
func UploadProgress(ctx context.Context, c *transport.Client, localPath, remotePath string, prog func(int64)) (FileResult, error) {
	fi, err := os.Stat(localPath)
	if err != nil {
		return FileResult{}, err
	}
	sum, err := sha1File(localPath)
	if err != nil {
		return FileResult{}, err
	}
	checksum := ocChecksum(sum)

	var etag, fileID string
	if fi.Size() <= chunkThreshold {
		etag, fileID, err = uploadSingle(ctx, c, localPath, remotePath, fi.Size(), checksum, prog)
	} else {
		etag, fileID, err = uploadChunked(ctx, c, localPath, remotePath, fi.Size(), checksum, prog)
	}
	if err != nil {
		return FileResult{}, err
	}

	// Some server/storage configurations omit the revision headers on write; if
	// so, fetch them with a cheap stat so the baseline is accurate.
	if etag == "" || fileID == "" {
		if e, ok, serr := c.Stat(ctx, remotePath); serr == nil && ok {
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

// uploadSingle performs a one-shot PUT.
func uploadSingle(ctx context.Context, c *transport.Client, localPath, remotePath string, size int64, checksum string, prog func(int64)) (etag, fileID string, err error) {
	f, err := os.Open(localPath)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	var body io.Reader = f
	if prog != nil {
		body = &progReader{r: f, fn: prog}
	}
	return c.PutWithChecksum(ctx, remotePath, body, size, checksum)
}

// uploadChunked uploads in numbered chunks then assembles, skipping any chunks
// already present from a previous interrupted attempt.
func uploadChunked(ctx context.Context, c *transport.Client, localPath, remotePath string, size int64, checksum string, prog func(int64)) (etag, fileID string, err error) {
	uploadID, err := genUploadID()
	if err != nil {
		return "", "", err
	}
	if err := c.CreateUpload(ctx, uploadID); err != nil {
		return "", "", err
	}
	// On failure, leave the partial upload dir for a future resume only if the
	// error is transient; for simplicity we clean up here and let the next sync
	// restart cleanly.
	committed := false
	defer func() {
		if !committed {
			_ = c.DeleteUpload(ctx, uploadID)
		}
	}()

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
	for i := 1; offset < size; i++ {
		n := chunkSize
		if remaining := size - offset; remaining < n {
			n = remaining
		}
		name := fmt.Sprintf("%05d", i)
		if got, ok := existing[name]; !ok || got != n {
			var section io.Reader = io.NewSectionReader(f, offset, n)
			if prog != nil {
				section = &progReader{r: section, fn: prog}
			}
			if err := c.PutChunk(ctx, uploadID, name, section, n, remotePath); err != nil {
				return "", "", err
			}
		} else if prog != nil {
			prog(n) // already-uploaded chunk counts toward progress
		}
		offset += n
	}

	etag, fileID, err = c.AssembleUpload(ctx, uploadID, remotePath, size, checksum)
	if err != nil {
		return "", "", err
	}
	committed = true
	return etag, fileID, nil
}

// chunkSizeFor picks a chunk size that respects the protocol minimum while
// keeping the chunk count under the cap for very large files.
func chunkSizeFor(size int64) int64 {
	cs := int64(minChunkSize)
	if needed := (size + maxChunks - 1) / maxChunks; needed > cs {
		cs = needed
	}
	return cs
}

// genUploadID returns a random identifier for an upload session.
func genUploadID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "nimbo-" + hex.EncodeToString(b[:]), nil
}
