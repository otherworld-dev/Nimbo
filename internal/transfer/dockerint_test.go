//go:build dockerint

// Integration tests against a real local Nextcloud (Docker). Opt-in — they need
// a server and are excluded from the normal suite:
//
//	go test -tags dockerint -run Docker ./internal/transfer/
//
// Defaults target the dev container on localhost:8080 with admin/admin; override
// with NIMBO_IT_URL / NIMBO_IT_USER / NIMBO_IT_PASS.
package transfer

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/otherworld/nimbo/internal/transport"
)

func itClient(t *testing.T) *transport.Client {
	t.Helper()
	url := envOr("NIMBO_IT_URL", "http://localhost:8080")
	user := envOr("NIMBO_IT_USER", "admin")
	pass := envOr("NIMBO_IT_PASS", "admin")
	return transport.New(url, user, pass)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// A chunked upload interrupted partway must RESUME against a real Nextcloud —
// the session and its uploaded chunks survive the failure, the second attempt
// finds them via the real uploads endpoint (MKCOL-already-exists + PROPFIND),
// skips them, and the real server-side assembly (with our OC-Checksum, which
// NC verifies) produces a byte-identical file. This is the "50-300GB upload
// never restarts from byte zero" guarantee from GitHub issue #1, proven
// against real Nextcloud semantics rather than a fake.
func TestDockerResumeAgainstRealNextcloud(t *testing.T) {
	c := itClient(t)
	ctx := context.Background()

	dir := t.TempDir()
	local := filepath.Join(dir, "resume.bin")
	const size = int64(35 << 20) // 4 chunks at the 10 MiB default
	writeRandom(t, local, size)
	wantSHA, err := sha1File(local)
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(local)
	uploadID := uploadIDFor("nimbo-itest-resume.bin", size, fi.ModTime().UnixNano())

	const remote = "nimbo-itest-resume.bin"
	t.Cleanup(func() {
		_ = c.Delete(context.Background(), remote)
		_ = c.DeleteUpload(context.Background(), uploadID)
	})

	// Pass 1: cancel once ~1.2 chunks are on the wire, leaving a partial session.
	cctx, cancel := context.WithCancel(ctx)
	var sent int64
	_, err = UploadProgress(cctx, c, local, remote, func(n int64) {
		if atomic.AddInt64(&sent, n) >= 12<<20 {
			cancel()
		}
	})
	if err == nil {
		t.Fatal("first pass should have been cancelled mid-upload, but it completed")
	}

	// The fix (no delete-on-failure): the session and its chunks must survive.
	existing, lerr := c.ListChunks(ctx, uploadID)
	if lerr != nil {
		t.Fatalf("ListChunks after cancel: %v", lerr)
	}
	if len(existing) == 0 {
		t.Fatal("no chunks survived the cancelled upload — resume impossible against real NC")
	}
	t.Logf("%d chunk(s) survived the interruption; resuming", len(existing))

	// Pass 2: resumes from the surviving session and completes.
	if _, err := Upload(ctx, c, local, remote); err != nil {
		t.Fatalf("resumed upload failed against real NC: %v", err)
	}

	// Byte-identical after a real server-side assembly.
	if got := downloadSHA(t, c, remote); got != wantSHA {
		t.Fatalf("assembled file sha1 %s != original %s — resume corrupted the file", got, wantSHA)
	}

	// A successful assembly consumes the session.
	if after, _ := c.ListChunks(ctx, uploadID); len(after) != 0 {
		t.Errorf("session not consumed after assembly: %d chunk(s) remain", len(after))
	}
}

// A fresh full upload to a real NC must land byte-identical, exercising the
// single-shot path's checksum verification too (files at/under the threshold).
func TestDockerSmallUploadIntegrity(t *testing.T) {
	c := itClient(t)
	ctx := context.Background()
	dir := t.TempDir()
	local := filepath.Join(dir, "small.bin")
	writeRandom(t, local, 64<<10) // 64 KiB — single PUT
	wantSHA, err := sha1File(local)
	if err != nil {
		t.Fatal(err)
	}
	const remote = "nimbo-itest-small.bin"
	t.Cleanup(func() { _ = c.Delete(context.Background(), remote) })

	if _, err := Upload(ctx, c, local, remote); err != nil {
		t.Fatalf("small upload failed: %v", err)
	}
	if got := downloadSHA(t, c, remote); got != wantSHA {
		t.Fatalf("uploaded file sha1 %s != original %s", got, wantSHA)
	}
}

func writeRandom(t *testing.T, path string, n int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := io.CopyN(f, rand.New(rand.NewSource(42)), n); err != nil {
		t.Fatal(err)
	}
}

func downloadSHA(t *testing.T, c *transport.Client, remote string) string {
	t.Helper()
	body, _, err := c.Get(context.Background(), remote)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	h := sha1.New()
	if _, err := io.Copy(h, body); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}
