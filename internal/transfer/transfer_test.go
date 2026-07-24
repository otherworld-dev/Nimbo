package transfer

import "testing"

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
