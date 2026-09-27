// Package transfer executes the engine's planned actions against the network and
// filesystem: resumable downloads, single-shot and chunked uploads, all with
// integrity checks and atomic local writes. It records each success in the
// baseline so subsequent syncs are incremental.
package transfer

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"strings"
)

// Nextcloud's default content checksum is SHA1, surfaced via the OC-Checksum
// header as "SHA1:<hexdigest>" (possibly alongside other algorithms).

// newHasher returns a fresh SHA1 hasher.
func newHasher() hash.Hash { return sha1.New() }

// sumHex finalises a hasher to a lowercase hex digest. It accepts any value
// that can produce a digest, so the download path can pass its narrower writer.
func sumHex(h interface{ Sum(b []byte) []byte }) string {
	return hex.EncodeToString(h.Sum(nil))
}

// sha1File computes the SHA1 of a file's contents as a lowercase hex string.
func sha1File(path string) (string, error) { return sha1FileCtx(context.Background(), path) }

// sha1FileCtx is sha1File that stops, with ctx's error, once ctx is done.
func sha1FileCtx(ctx context.Context, path string) (string, error) {
	f, err := openShared(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := newHasher()
	if _, err := io.Copy(h, &hashReader{ctx: ctx, r: f, what: "file"}); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return sumHex(h), nil
}

// testHookHashRead, when set, runs before each read a hash pass makes, with
// what is being hashed: "file" (a whole file before it is uploaded), "chunk"
// (a chunk a resumed upload already has on the server) or "part" (a resumed
// download's .nimbo-part). Tests cancel a transfer mid-hash with it.
var testHookHashRead func(what string)

// hashReader feeds a hash pass. Hashing a 300 GB file takes minutes, at the
// start of every attempt, and a transfer that is paused, set aside or stopped
// must not make whoever stopped it wait that long (Deck #702): once ctx is
// done, the next read fails with ctx's error.
type hashReader struct {
	ctx  context.Context
	r    io.Reader
	what string
}

func (h *hashReader) Read(p []byte) (int, error) {
	if testHookHashRead != nil {
		testHookHashRead(h.what)
	}
	if err := h.ctx.Err(); err != nil {
		return 0, err
	}
	return h.r.Read(p)
}

// SHA1File returns the lowercase hex SHA1 of a file's contents. Exported for the
// rename detector's local-hash callback in the CLI.
func SHA1File(path string) (string, error) { return sha1File(path) }

// ocChecksum formats a SHA1 hex digest as an OC-Checksum header value.
func ocChecksum(sha1hex string) string {
	if sha1hex == "" {
		return ""
	}
	return "SHA1:" + sha1hex
}

// parseSHA1 extracts the SHA1 hex digest from an OC-Checksum header value, which
// may contain several space-separated "ALGO:digest" tokens. ok is false if no
// SHA1 token is present.
func parseSHA1(header string) (digest string, ok bool) {
	for _, tok := range strings.Fields(header) {
		if strings.HasPrefix(strings.ToUpper(tok), "SHA1:") {
			return strings.ToLower(tok[len("SHA1:"):]), true
		}
	}
	return "", false
}
