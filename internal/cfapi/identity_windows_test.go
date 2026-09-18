//go:build windows

package cfapi

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPlaceholderIdentityLive reads back the identity CreatePlaceholders
// stamped on a file and a directory placeholder, and refuses a plain file.
// Live-driver test, opt in with NIMBO_CFAPI_LIVE=1.
func TestPlaceholderIdentityLive(t *testing.T) {
	if os.Getenv("NIMBO_CFAPI_LIVE") == "" {
		t.Skip("set NIMBO_CFAPI_LIVE=1 to run against the real Cloud Files API")
	}
	if !Supported() {
		t.Skip("Cloud Files API not available")
	}
	root := filepath.Join(t.TempDir(), "idroot")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Purge(root) })

	hydrate := func(identity []byte, offset, length int64) ([]byte, error) {
		return make([]byte, length), nil
	}
	list := func(rel string) []PlaceholderInfo { return []PlaceholderInfo{} }
	connKey, err := Mount(root, "NimboIdTest", "", hydrate, list)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	defer Unmount(root, connKey)

	if err := CreatePlaceholders(root, []PlaceholderInfo{
		{Name: "f.bin", Size: 16, ModTime: time.Now(), Identity: []byte("remote/dir/f.bin")},
		{Name: "d", IsDir: true, ModTime: time.Now(), Identity: []byte("remote/dir/d")},
	}); err != nil {
		t.Fatalf("CreatePlaceholders: %v", err)
	}
	for name, want := range map[string]string{"f.bin": "remote/dir/f.bin", "d": "remote/dir/d"} {
		got, err := PlaceholderIdentity(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("PlaceholderIdentity(%s): %v", name, err)
		}
		if string(got) != want {
			t.Errorf("identity of %s = %q, want %q", name, got, want)
		}
	}

	plain := filepath.Join(root, "plain.txt")
	if err := os.WriteFile(plain, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PlaceholderIdentity(plain); !errors.Is(err, ErrNotPlaceholder) {
		t.Errorf("plain file: err = %v, want ErrNotPlaceholder", err)
	}
}
