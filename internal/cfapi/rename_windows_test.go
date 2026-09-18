//go:build windows

package cfapi

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRenameCompletionReportsMoves pins the fix for issue #7: the filter's
// rename-completion callback must report a cross-directory move of an
// online-only placeholder (old, new), a same-directory rename, and a move of
// a placeholder directory. Renames must come from ANOTHER process — the
// filter does not report renames made by the connected provider's own
// process (measured 2026-09-14) — and only placeholders are reported, not
// plain files/directories. Live-driver test, opt in with NIMBO_CFAPI_LIVE=1.
func TestRenameCompletionReportsMoves(t *testing.T) {
	if os.Getenv("NIMBO_CFAPI_LIVE") == "" {
		t.Skip("set NIMBO_CFAPI_LIVE=1 to run against the real Cloud Files API")
	}
	if !Supported() {
		t.Skip("Cloud Files API not available")
	}
	root := filepath.Join(t.TempDir(), "mvroot")
	// "other" is a plain directory — the target parent needn't be a
	// placeholder. "sub" is created as a placeholder directory below, after
	// Mount, since CfCreatePlaceholders needs a connected sync root.
	for _, d := range []string{root, filepath.Join(root, "other")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = Purge(root) })

	hydrate := func(identity []byte, offset, length int64) ([]byte, error) {
		return make([]byte, length), nil
	}
	list := func(rel string) []PlaceholderInfo { return []PlaceholderInfo{} }
	connKey, err := Mount(root, "NimboMoveTest", "", hydrate, list)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	defer Unmount(root, connKey)

	if err := CreatePlaceholders(root, []PlaceholderInfo{
		{Name: "x.bin", Size: 8, ModTime: time.Now(), Identity: []byte("remote/x.bin")},
		{Name: "sub", IsDir: true, ModTime: time.Now(), Identity: []byte("remote/sub")},
	}); err != nil {
		t.Fatalf("CreatePlaceholders: %v", err)
	}
	got := make(chan [2]string, 8)
	SetRenameHandler(connKey, func(oldPath, newPath string) { got <- [2]string{oldPath, newPath} })

	// mv performs src -> dst from ANOTHER process (cmd.exe), never in-process:
	// the filter suppresses rename-completion notifications for renames made
	// by the connected provider's own process (measured 2026-09-14).
	mv := func(src, dst string) error {
		out, err := exec.Command("cmd.exe", "/c", "move", "/Y", src, dst).CombinedOutput()
		if err != nil {
			return fmt.Errorf("move %q -> %q: %v: %s", src, dst, err, out)
		}
		return nil
	}

	expect := func(step, src, dst string) {
		t.Helper()
		if err := mv(src, dst); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		select {
		case ev := <-got:
			if !strings.EqualFold(ev[0], src) || !strings.EqualFold(ev[1], dst) {
				t.Fatalf("%s: callback reported %q -> %q, want %q -> %q", step, ev[0], ev[1], src, dst)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: no rename-completion callback", step)
		}
	}

	// 1. cross-directory move of an online-only placeholder
	expect("move", filepath.Join(root, "x.bin"), filepath.Join(root, "sub", "x.bin"))
	// 2. same-directory rename
	expect("rename", filepath.Join(root, "sub", "x.bin"), filepath.Join(root, "sub", "y.bin"))
	// 3. directory move (one callback for the directory, none per child)
	expect("dir move", filepath.Join(root, "sub"), filepath.Join(root, "other", "sub"))
	select {
	case ev := <-got:
		t.Fatalf("unexpected extra callback %v", ev)
	case <-time.After(1 * time.Second):
	}
	if _, err := PlaceholderIdentity(filepath.Join(root, "other", "sub", "y.bin")); err != nil {
		t.Fatalf("moved placeholder lost its cloud state: %v", err)
	}

	// 4. an in-process rename must NOT be reported — the filter only reports
	// renames from another process (measured 2026-09-14). This suits Nimbo:
	// its own renames are down-sync pulls the watcher already suppresses.
	inProcSrc := filepath.Join(root, "other", "sub", "y.bin")
	inProcDst := filepath.Join(root, "other", "sub", "z.bin")
	if err := os.Rename(inProcSrc, inProcDst); err != nil {
		t.Fatalf("in-process rename: %v", err)
	}
	select {
	case ev := <-got:
		t.Fatalf("in-process rename unexpectedly produced a callback %v", ev)
	case <-time.After(1 * time.Second):
	}
}
