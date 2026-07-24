package transfer

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/otherworld/nimbo/internal/transport"
)

// partSuffix marks an in-progress download. It matches the engine's local-scan
// ignore list, so partial files never influence sync decisions.
const partSuffix = ".nimbo-part"

// FileResult is the post-transfer state of a file, used to update the baseline.
type FileResult struct {
	ETag        string
	FileID      string
	Size        int64
	MTimeNanos  int64
	ContentSHA1 string
}

// Download fetches remotePath into localPath. It resumes a prior partial
// download via HTTP Range, streams to a temp file while computing the content
// hash, verifies that hash against the server's OC-Checksum when provided, sets
// the local mtime to the server's, and only then atomically renames the temp
// file into place. A checksum mismatch fails the transfer and discards the temp.
func Download(ctx context.Context, c *transport.Client, remotePath, localPath string) (FileResult, error) {
	return DownloadProgress(ctx, c, remotePath, localPath, nil)
}

// DownloadProgress is Download with an optional progress callback invoked with
// the number of bytes received as they stream in.
func DownloadProgress(ctx context.Context, c *transport.Client, remotePath, localPath string, prog func(int64)) (FileResult, error) {
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return FileResult{}, err
	}
	part := localPath + partSuffix

	offset, hasher, err := resumeState(part)
	if err != nil {
		return FileResult{}, err
	}

	body, hdr, status, err := c.GetFrom(ctx, remotePath, offset)
	if err != nil {
		return FileResult{}, err
	}
	defer body.Close()

	// If we asked to resume but the server sent the whole file, start over.
	flag := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if offset > 0 && status == http.StatusOK {
		offset = 0
		hasher = newHasher()
		flag = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	}

	f, err := os.OpenFile(part, flag, 0o644)
	if err != nil {
		return FileResult{}, err
	}
	// Tee the network stream through the hasher (and progress meter) as we write.
	writers := []io.Writer{f, hasher}
	if prog != nil {
		writers = append(writers, &progWriter{fn: prog})
	}
	if _, err := io.Copy(io.MultiWriter(writers...), body); err != nil {
		f.Close()
		return FileResult{}, fmt.Errorf("download %s: %w", remotePath, err)
	}
	if err := f.Close(); err != nil {
		return FileResult{}, err
	}

	if want, ok := parseSHA1(hdr.Get("OC-Checksum")); ok {
		if got := sumHex(hasher); got != want {
			os.Remove(part)
			return FileResult{}, fmt.Errorf("download %s: checksum mismatch (got %s, want %s)", remotePath, got, want)
		}
	}

	// Set local mtime to the server's so the next diff sees no spurious change.
	mtime := time.Now()
	if t, err := http.ParseTime(hdr.Get("Last-Modified")); err == nil {
		mtime = t
		_ = os.Chtimes(part, t, t)
	}

	// Clear any read-only attribute on an existing target so the rename can
	// overwrite it (a previously mirrored read-only file being updated). The
	// executor re-applies read-only afterwards if the server still says so.
	_ = os.Chmod(localPath, 0o644)
	if err := os.Rename(part, localPath); err != nil {
		return FileResult{}, fmt.Errorf("commit download %s: %w", localPath, err)
	}

	fi, err := os.Stat(localPath)
	if err != nil {
		return FileResult{}, err
	}
	return FileResult{
		ETag:        headerETag(hdr),
		FileID:      hdr.Get("OC-FileId"),
		Size:        fi.Size(),
		MTimeNanos:  mtime.UnixNano(),
		ContentSHA1: sumHex(hasher),
	}, nil
}

// headerETag returns the response ETag, preferring Nextcloud's OC-ETag.
func headerETag(hdr http.Header) string {
	if e := strings.Trim(hdr.Get("OC-ETag"), `"`); e != "" {
		return e
	}
	return strings.Trim(hdr.Get("ETag"), `"`)
}

// resumeState inspects an existing partial file and returns the byte offset to
// resume from plus a hasher seeded with the bytes already on disk. A missing
// part file yields offset 0 and a fresh hasher.
func resumeState(part string) (int64, hashWriter, error) {
	h := newHasher()
	fi, err := os.Stat(part)
	if os.IsNotExist(err) {
		return 0, h, nil
	}
	if err != nil {
		return 0, nil, err
	}
	f, err := os.Open(part)
	if err != nil {
		return 0, nil, err
	}
	defer f.Close()
	if _, err := io.Copy(h, f); err != nil {
		return 0, nil, fmt.Errorf("seed resume hash: %w", err)
	}
	return fi.Size(), h, nil
}

// hashWriter is the subset of hash.Hash the download path uses.
type hashWriter = interface {
	io.Writer
	Sum(b []byte) []byte
}
