//go:build windows

package cfapi

import (
	"os"
	"path/filepath"
	"testing"
)

// TestExcludeFromSync pins the fix for the last icon class: sync-excluded
// LOCAL-ONLY files (the official client's .sync_*.db / .nextcloudsync.log,
// Desktop.ini) are plain files inside a cloud root, which Explorer natively
// renders as forever-pending arrows. CF_PIN_STATE_EXCLUDED is the platform's
// designed vocabulary for "not synced, on purpose" — VM-verified 2026-08-19:
// an excluded item's Status cell goes BLANK. ExcludeFromSync converts the
// plain file in place and excludes it; content untouched; idempotent.
//
// Live-driver test, opt in with NIMBO_CFAPI_LIVE=1.
func TestExcludeFromSync(t *testing.T) {
	if os.Getenv("NIMBO_CFAPI_LIVE") == "" {
		t.Skip("set NIMBO_CFAPI_LIVE=1 to run against the real Cloud Files API")
	}
	if !Supported() {
		t.Skip("Cloud Files API not available")
	}
	root := filepath.Join(t.TempDir(), "exclroot")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Purge(root) })
	list := func(rel string) []PlaceholderInfo { return nil }
	hydrate := func(identity []byte, offset, length int64) ([]byte, error) {
		return make([]byte, length), nil
	}
	connKey, err := Mount(root, "NimboExclTest", "", hydrate, list)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	defer Unmount(root, connKey)

	journal := filepath.Join(root, ".fake_sync.db")
	if err := os.WriteFile(journal, []byte("journal bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ExcludeFromSync(journal); err != nil {
		t.Fatalf("ExcludeFromSync: %v", err)
	}
	attrs, tag, err := findAttrTag(journal)
	if err != nil {
		t.Fatal(err)
	}
	if attrs&0x400 == 0 {
		t.Errorf("not converted to a placeholder (attrs=0x%x)", attrs)
	}
	_ = tag
	if b, rerr := os.ReadFile(journal); rerr != nil || string(b) != "journal bytes" {
		t.Fatalf("content damaged: %v", rerr)
	}
	// Idempotent: a second call must not error or change anything.
	if err := ExcludeFromSync(journal); err != nil {
		t.Fatalf("second ExcludeFromSync: %v", err)
	}
}
