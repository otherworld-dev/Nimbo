package transfer

import (
	"crypto/sha1"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/otherworld/nimbo/internal/engine"
)

// TestRedundantDownload covers the metadata-only remote change: taking or
// releasing a files_lock lock bumps the ETag without touching content, and the
// diff can only see the ETag. Without this guard every peer re-downloads the
// whole file twice per lock cycle — and in on-demand mode the same signal
// dehydrates a file whose bytes never changed.
func TestRedundantDownload(t *testing.T) {
	dir := t.TempDir()
	body := []byte("hello world")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha1.Sum(body)
	digest := hex.EncodeToString(sum[:])
	e := &Executor{LocalRoot: dir}

	remote := func(r engine.RemoteState) { e.Remote = map[string]engine.RemoteState{"a.txt": r} }

	t.Run("identical content is redundant", func(t *testing.T) {
		remote(engine.RemoteState{Path: "a.txt", Size: int64(len(body)), SHA1: digest, ETag: "new"})
		got, redundant := e.redundantDownload("a.txt")
		if !redundant {
			t.Fatal("redundant = false, want true")
		}
		if got != digest {
			t.Errorf("returned sha %q, want the local hash %q", got, digest)
		}
	})

	t.Run("different content must download", func(t *testing.T) {
		remote(engine.RemoteState{Path: "a.txt", Size: int64(len(body)), SHA1: "00000000000000000000000000000000000000aa"})
		if _, redundant := e.redundantDownload("a.txt"); redundant {
			t.Error("redundant = true, want false")
		}
	})

	t.Run("no server checksum must download", func(t *testing.T) {
		remote(engine.RemoteState{Path: "a.txt", Size: int64(len(body))})
		if _, redundant := e.redundantDownload("a.txt"); redundant {
			t.Error("redundant = true, want false — never guess without a checksum")
		}
	})

	t.Run("size mismatch must download", func(t *testing.T) {
		remote(engine.RemoteState{Path: "a.txt", Size: 999, SHA1: digest})
		if _, redundant := e.redundantDownload("a.txt"); redundant {
			t.Error("redundant = true, want false")
		}
	})

	t.Run("missing local file must download", func(t *testing.T) {
		remote(engine.RemoteState{Path: "gone.txt", Size: 1, SHA1: digest})
		e.Remote["gone.txt"] = e.Remote["a.txt"]
		if _, redundant := e.redundantDownload("gone.txt"); redundant {
			t.Error("redundant = true, want false")
		}
	})

	t.Run("directory is never redundant", func(t *testing.T) {
		remote(engine.RemoteState{Path: "a.txt", IsDir: true, SHA1: digest})
		if _, redundant := e.redundantDownload("a.txt"); redundant {
			t.Error("redundant = true, want false")
		}
	})

	t.Run("unknown path must download", func(t *testing.T) {
		remote(engine.RemoteState{Path: "a.txt", Size: int64(len(body)), SHA1: digest})
		if _, redundant := e.redundantDownload("nosuch.txt"); redundant {
			t.Error("redundant = true, want false")
		}
	})
}

func TestParseSHA1(t *testing.T) {
	tests := []struct {
		header string
		want   string
		ok     bool
	}{
		{"SHA1:abc123", "abc123", true},
		{"sha1:ABC123", "abc123", true}, // case-insensitive algo, lowercased digest
		{"MD5:deadbeef SHA1:cafe", "cafe", true},
		{"ADLER32:0001 MD5:dead", "", false},
		{"", "", false},
	}
	for _, tc := range tests {
		got, ok := parseSHA1(tc.header)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseSHA1(%q) = (%q,%v), want (%q,%v)", tc.header, got, ok, tc.want, tc.ok)
		}
	}
}

func TestOCChecksum(t *testing.T) {
	if got := ocChecksum("abc"); got != "SHA1:abc" {
		t.Errorf("ocChecksum = %q", got)
	}
	if got := ocChecksum(""); got != "" {
		t.Errorf("ocChecksum(empty) = %q, want empty", got)
	}
}

func TestChunkSizeFor(t *testing.T) {
	// Small/medium files use the minimum chunk size.
	if cs := chunkSizeFor(50 << 20); cs != minChunkSize {
		t.Errorf("chunkSizeFor(50MiB) = %d, want %d", cs, minChunkSize)
	}
	// Very large files scale chunk size up to stay within maxChunks.
	huge := int64(maxChunks) * minChunkSize * 3 // 3x what min chunks could cover
	cs := chunkSizeFor(huge)
	if n := (huge + cs - 1) / cs; n > maxChunks {
		t.Errorf("chunkSizeFor(%d) yields %d chunks, exceeds cap %d", huge, n, maxChunks)
	}
	if cs < minChunkSize {
		t.Errorf("chunk size %d below minimum %d", cs, minChunkSize)
	}
}
