//go:build windows

package cfapi

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestRootFetchStorm reproduces the #569 spin loop and proves the fix.
//
// THE BUG, from the VM's production log (2026-08-18 01:17): on every mount of
// an already-populated folder, the filter issues FETCH_PLACEHOLDERS for the
// ROOT — ignoring CF_REGISTER_FLAG_DISABLE_ON_DEMAND_POPULATION_ON_ROOT — and
// our answer (the folder's existing entries) fails per entry with
// ERROR_ALREADY_EXISTS, so the filter re-issues the same request every ~700ms
// forever. Each round is a server PROPFIND: a slow-motion repeat of the
// PROPFIND DoS incident, plus "Offline" flapping when the server throttles.
//
// THE FIX CANDIDATE: make the root itself a placeholder that is already
// fully populated (CfConvertToPlaceholder defaults to population-disabled)
// and in-sync, so the filter has no reason to ever ask for root population.
//
// Live-driver test, opt in with NIMBO_CFAPI_LIVE=1.
func TestRootFetchStorm(t *testing.T) {
	if os.Getenv("NIMBO_CFAPI_LIVE") == "" {
		t.Skip("set NIMBO_CFAPI_LIVE=1 to run against the real Cloud Files API")
	}
	if !Supported() {
		t.Skip("Cloud Files API not available")
	}

	mountWithRealContent := func(t *testing.T, convertRoot bool) (root string, rootFetches *atomic.Int32, cleanup func()) {
		t.Helper()
		root = filepath.Join(t.TempDir(), "stormroot")
		// Simulate the VM: a folder that is ALREADY populated with real content
		// before the mount (the state every remount of a used folder is in).
		if err := os.MkdirAll(filepath.Join(root, "SubA"), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{"one.txt", "two.txt", "SubA/inner.txt"} {
			if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(f)), []byte("data"), 0o644); err != nil {
				t.Fatal(err)
			}
		}

		var fetches atomic.Int32
		oldDebug := Debug
		Debug = func(format string, args ...any) {
			if strings.Contains(format, "FETCH_PLACEHOLDERS rel=") && len(args) > 0 {
				if rel, ok := args[0].(string); ok && rel == "" {
					fetches.Add(1)
				}
			}
		}

		list := func(rel string) []PlaceholderInfo {
			if rel == "" {
				return []PlaceholderInfo{
					{Name: "one.txt", Size: 4, ModTime: time.Now(), Identity: []byte("remote/one.txt")},
					{Name: "two.txt", Size: 4, ModTime: time.Now(), Identity: []byte("remote/two.txt")},
					{Name: "SubA", IsDir: true, ModTime: time.Now(), Identity: []byte("remote/SubA")},
				}
			}
			return []PlaceholderInfo{}
		}
		hydrate := func(identity []byte, offset, length int64) ([]byte, error) {
			return make([]byte, length), nil
		}
		connKey, err := Mount(root, "NimboStormTest", "", hydrate, list)
		if err != nil {
			t.Fatalf("Mount: %v", err)
		}
		if convertRoot {
			// THE FIX under test: root becomes an in-sync placeholder whose
			// population is already complete.
			if err := MarkInSync(root, []byte("remote-root")); err != nil {
				t.Fatalf("convert root: %v", err)
			}
		}
		return root, &fetches, func() {
			Unmount(root, connKey)
			Debug = oldDebug
			_ = Purge(root)
		}
	}

	stormScore := func(t *testing.T, root string, fetches *atomic.Int32) int32 {
		t.Helper()
		// Enumerate, then WAIT — refires arrive on their own cadence (~700ms in
		// the field). A healthy mount stays at 0 or 1 total.
		if _, err := os.ReadDir(root); err != nil {
			t.Fatalf("ReadDir(root): %v", err)
		}
		time.Sleep(3 * time.Second)
		if _, err := os.ReadDir(root); err != nil {
			t.Fatalf("ReadDir(root) again: %v", err)
		}
		time.Sleep(2 * time.Second)
		return fetches.Load()
	}

	t.Run("baseline: current registration storms", func(t *testing.T) {
		root, fetches, cleanup := mountWithRealContent(t, false)
		defer cleanup()
		n := stormScore(t, root, fetches)
		t.Logf("root FETCH_PLACEHOLDERS count over ~5s: %d", n)
		if n <= 2 {
			t.Logf("NOTE: storm did not reproduce here (n=%d) — machine-dependent; the fix run below must still stay quiet", n)
		}
	})

	t.Run("fix A: register without the root-disable flag", func(t *testing.T) {
		testRootRegisterFlags = cfRegisterFlagUpdate
		defer func() { testRootRegisterFlags = 0 }()
		root, fetches, cleanup := mountWithRealContent(t, false)
		defer cleanup()
		n := stormScore(t, root, fetches)
		t.Logf("root FETCH_PLACEHOLDERS count over ~5s: %d", n)
		if n > 2 {
			t.Errorf("fix A insufficient: root still fetched %d times", n)
		}
	})

	t.Run("fix A2: no root-disable flag AND root converted", func(t *testing.T) {
		testRootRegisterFlags = cfRegisterFlagUpdate
		defer func() { testRootRegisterFlags = 0 }()
		root, fetches, cleanup := mountWithRealContent(t, true)
		defer cleanup()
		n := stormScore(t, root, fetches)
		t.Logf("root FETCH_PLACEHOLDERS count over ~5s: %d", n)
		if n > 1 {
			t.Errorf("fix A2 insufficient: root still fetched %d times", n)
		}
		if _, err := os.ReadFile(filepath.Join(root, "one.txt")); err != nil {
			t.Errorf("read one.txt after fix: %v", err)
		}
	})

	t.Run("fix B: root converted to populated placeholder", func(t *testing.T) {
		root, fetches, cleanup := mountWithRealContent(t, true)
		defer cleanup()
		if ph, _ := IsPlaceholder(root); !ph {
			t.Fatal("setup: root did not convert to a placeholder")
		}
		n := stormScore(t, root, fetches)
		t.Logf("root FETCH_PLACEHOLDERS count over ~5s: %d", n)
		if n > 1 {
			t.Errorf("fix insufficient: root still fetched %d times", n)
		}
		// The mount must still WORK: files hydratable, subdir enumerable.
		if _, err := os.ReadFile(filepath.Join(root, "one.txt")); err != nil {
			t.Errorf("read one.txt after fix: %v", err)
		}
		if entries, err := os.ReadDir(filepath.Join(root, "SubA")); err != nil || len(entries) == 0 {
			t.Errorf("SubA enumeration after fix: entries=%d err=%v", len(entries), err)
		}
	})
}
