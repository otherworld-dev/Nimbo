// Package transfer executes the engine's planned actions against the network and
// filesystem: resumable downloads, single-shot and chunked uploads, all with
// integrity checks and atomic local writes. It records each success in the
// baseline so subsequent syncs are incremental.
package transfer

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
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
func sha1File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := newHasher()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return sumHex(h), nil
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
