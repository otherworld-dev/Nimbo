//go:build windows

package cfapi

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// TestFlattenOnDisconnectMatrix isolates WHICH registration-policy delta stops
// a hydrated placeholder flattening to a plain file across a provider
// disconnect (Deck #580). OneDrive's live root (probed 2026-08-18) registers
// HydrationPolicy modifier 0x9 (VALIDATION_REQUIRED|ALLOW_FULL_RESTART),
// InSyncPolicy 0x111 and HardLinkPolicy 1; we register zeroes for all three,
// and our hydrated placeholders flatten while OneDrive's survive.
//
// Live-driver experiment, opt in with NIMBO_CFAPI_LIVE=1. Each variant is a
// fresh root: mount, seed one placeholder, hydrate it, disconnect, measure.
func TestFlattenOnDisconnectMatrix(t *testing.T) {
	if os.Getenv("NIMBO_CFAPI_LIVE") == "" {
		t.Skip("set NIMBO_CFAPI_LIVE=1 to run against the real Cloud Files API")
	}
	if !Supported() {
		t.Skip("Cloud Files API not available")
	}

	type policy struct {
		HydrationModifier uint16
		PopulationPrimary uint16
		InSyncPolicy      uint32
		HardLinkPolicy    uint32
	}
	variants := []struct {
		name string
		p    *policy
	}{
		{"baseline-zeroes", nil},
		{"insync-0x111", &policy{InSyncPolicy: 0x111}},
		{"hardlink-1", &policy{HardLinkPolicy: 1}},
		{"hydmod-0x8-restart", &policy{HydrationModifier: 0x8}},
		{"hydmod-0x1-validation", &policy{HydrationModifier: 0x1}},
		{"onedrive-all", &policy{HydrationModifier: 0x9, InSyncPolicy: 0x111, HardLinkPolicy: 1}},
		{"population-alwaysfull", &policy{PopulationPrimary: 3}},
		{"onedrive-all-pop", &policy{HydrationModifier: 0x9, PopulationPrimary: 3, InSyncPolicy: 0x111, HardLinkPolicy: 1}},
	}

	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			if v.p != nil {
				testRootPolicyOverride = (*struct {
					HydrationModifier uint16
					PopulationPrimary uint16
					InSyncPolicy      uint32
					HardLinkPolicy    uint32
				})(v.p)
			}
			defer func() { testRootPolicyOverride = nil }()

			root := filepath.Join(t.TempDir(), "flatroot")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = Purge(root) })

			list := func(rel string) []PlaceholderInfo { return nil }
			hydrate := func(identity []byte, offset, length int64) ([]byte, error) {
				return make([]byte, length), nil
			}
			connKey, err := Mount(root, "NimboFlatTest", "", hydrate, list)
			if err != nil {
				t.Fatalf("Mount: %v", err)
			}
			if err := CreatePlaceholders(root, []PlaceholderInfo{
				{Name: "keep.bin", Size: 8, ModTime: time.Now(), Identity: []byte("remote/keep.bin")},
			}); err != nil {
				Unmount(root, connKey)
				t.Fatalf("CreatePlaceholders: %v", err)
			}
			file := filepath.Join(root, "keep.bin")
			if _, err := os.ReadFile(file); err != nil {
				Unmount(root, connKey)
				t.Fatalf("hydrate: %v", err)
			}

			Disconnect(root, connKey)
			time.Sleep(2 * time.Second)

			attrs, _, aerr := findAttrTag(file)
			if aerr != nil {
				t.Fatalf("findAttrTag: %v", aerr)
			}
			survived := attrs&(0x400000|0x400) != 0
			t.Logf("variant=%s attrs=0x%x survived=%v", v.name, attrs, survived)
		})
	}
}

// TestFlattenStateMatrix isolates which PLACEHOLDER STATE survives a
// disconnect: the policy matrix showed no registration delta matters, so the
// filter must be deciding per file. Known so far: dehydrated survives, hydrated
// in-sync flattens. Variants probe pin state and the in-sync bit.
func TestFlattenStateMatrix(t *testing.T) {
	if os.Getenv("NIMBO_CFAPI_LIVE") == "" {
		t.Skip("set NIMBO_CFAPI_LIVE=1 to run against the real Cloud Files API")
	}
	if !Supported() {
		t.Skip("Cloud Files API not available")
	}

	clearInSync := func(t *testing.T, path string) {
		pathW, _ := windows.UTF16PtrFromString(path)
		h, err := windows.CreateFile(pathW, windows.GENERIC_READ|windows.GENERIC_WRITE,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
		if err != nil {
			t.Fatalf("open for clear-insync: %v", err)
		}
		defer windows.CloseHandle(h)
		var usn int64
		hr, _, _ := procCfSetInSyncState.Call(uintptr(h), 0 /* NOT_IN_SYNC */, 0, uintptr(unsafe.Pointer(&usn)))
		if int32(hr) < 0 {
			t.Fatalf("CfSetInSyncState(clear): 0x%08x", uint32(hr))
		}
	}

	// prep acts on the seeded placeholder (file) and returns the path whose
	// survival the variant measures.
	variants := []struct {
		name string
		prep func(t *testing.T, file string) string
	}{
		{"dehydrated-control", func(t *testing.T, file string) string { return file }},
		{"hydrated", func(t *testing.T, file string) string {
			if _, err := os.ReadFile(file); err != nil {
				t.Fatalf("hydrate: %v", err)
			}
			return file
		}},
		{"hydrated-pinned", func(t *testing.T, file string) string {
			if _, err := os.ReadFile(file); err != nil {
				t.Fatalf("hydrate: %v", err)
			}
			if err := SetPinState(file, true, false); err != nil {
				t.Fatalf("pin: %v", err)
			}
			return file
		}},
		{"pinned-only", func(t *testing.T, file string) string {
			if err := SetPinState(file, true, false); err != nil {
				t.Fatalf("pin: %v", err)
			}
			time.Sleep(1 * time.Second) // give pin-driven hydration a beat
			return file
		}},
		{"hydrated-notinsync", func(t *testing.T, file string) string {
			if _, err := os.ReadFile(file); err != nil {
				t.Fatalf("hydrate: %v", err)
			}
			clearInSync(t, file)
			return file
		}},
		// A CONVERTED placeholder never went through our FETCH_DATA/TRANSFER_DATA
		// path: it was a real file stamped with cloud state (the adopt/heal
		// conversion). If it survives while transfer-hydrated ones flatten, the
		// bug is our hydration completion bookkeeping (#569's struct-layout
		// class); if it flattens too, hydrated placeholders are simply disposable
		// to cldflt without a provider, full stop.
		{"converted-file", func(t *testing.T, file string) string {
			conv := filepath.Join(filepath.Dir(file), "real.bin")
			if err := os.WriteFile(conv, []byte("12345678"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := MarkInSync(conv, []byte("remote/real.bin")); err != nil {
				t.Fatalf("convert: %v", err)
			}
			return conv
		}},
		{"converted-dir", func(t *testing.T, file string) string {
			dir := filepath.Join(filepath.Dir(file), "realdir")
			if err := os.MkdirAll(filepath.Join(dir, "inner"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := MarkInSync(dir, []byte("remote/realdir")); err != nil {
				t.Fatalf("convert dir: %v", err)
			}
			return dir
		}},
	}

	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "stateroot")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = Purge(root) })
			list := func(rel string) []PlaceholderInfo { return nil }
			hydrate := func(identity []byte, offset, length int64) ([]byte, error) {
				return make([]byte, length), nil
			}
			connKey, err := Mount(root, "NimboFlatTest", "", hydrate, list)
			if err != nil {
				t.Fatalf("Mount: %v", err)
			}
			if err := CreatePlaceholders(root, []PlaceholderInfo{
				{Name: "keep.bin", Size: 8, ModTime: time.Now(), Identity: []byte("remote/keep.bin")},
			}); err != nil {
				Unmount(root, connKey)
				t.Fatalf("CreatePlaceholders: %v", err)
			}
			file := filepath.Join(root, "keep.bin")
			measure := v.prep(t, file)

			Disconnect(root, connKey)
			time.Sleep(2 * time.Second)

			// TWO probes. findAttrTag sees the DISGUISED view (after disconnect
			// this process is no longer a sync engine, and cfapi hides reparse
			// points from non-exempt processes — hydrated placeholders then LOOK
			// like plain files). fsutil lives under %systemroot% and is exempt,
			// so its answer is the on-disk truth.
			attrs, _, aerr := findAttrTag(measure)
			if aerr != nil {
				t.Fatalf("findAttrTag: %v", aerr)
			}
			out, _ := exec.Command("fsutil", "reparsepoint", "query", measure).CombinedOutput()
			truth := "REPARSE PRESENT"
			if !strings.Contains(string(out), "Tag value") {
				truth = "NO REPARSE (genuinely plain): " + strings.TrimSpace(string(out))
			}
			t.Logf("variant=%s disguisedAttrs=0x%x fsutil=%s", v.name, attrs, truth)
		})
	}
}

// TestFlattenWithoutDisconnect is the other half of the matrix: does a hydrated
// placeholder survive when the provider process EXITS WITHOUT calling
// CfDisconnectSyncRoot? OneDrive's placeholders survive its exit, and OneDrive
// may simply never disconnect — if the hard-exit path preserves state while our
// graceful Disconnect strips it, the "fix" for #580 is to stop disconnecting.
//
// The child half runs in a subprocess (a graceful in-process skip of Disconnect
// would still run the Go runtime's exit, not a real provider death).
func TestFlattenWithoutDisconnect(t *testing.T) {
	if os.Getenv("NIMBO_FLATTEN_CHILD") != "" {
		// CHILD: mount, seed, hydrate, exit hard. No Disconnect, no cleanup.
		root := os.Getenv("NIMBO_FLATTEN_ROOT")
		list := func(rel string) []PlaceholderInfo { return nil }
		hydrate := func(identity []byte, offset, length int64) ([]byte, error) {
			return make([]byte, length), nil
		}
		if _, err := Mount(root, "NimboFlatTest", "", hydrate, list); err != nil {
			os.Exit(3)
		}
		if err := CreatePlaceholders(root, []PlaceholderInfo{
			{Name: "keep.bin", Size: 8, ModTime: time.Now(), Identity: []byte("remote/keep.bin")},
		}); err != nil {
			os.Exit(4)
		}
		if _, err := os.ReadFile(filepath.Join(root, "keep.bin")); err != nil {
			os.Exit(5)
		}
		os.Exit(0)
	}

	if os.Getenv("NIMBO_CFAPI_LIVE") == "" {
		t.Skip("set NIMBO_CFAPI_LIVE=1 to run against the real Cloud Files API")
	}
	if !Supported() {
		t.Skip("Cloud Files API not available")
	}
	root := filepath.Join(t.TempDir(), "hardexitroot")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Purge(root) })

	cmd := exec.Command(os.Args[0], "-test.run", "TestFlattenWithoutDisconnect")
	cmd.Env = append(os.Environ(), "NIMBO_FLATTEN_CHILD=1", "NIMBO_FLATTEN_ROOT="+root)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child failed: %v\n%s", err, out)
	}
	time.Sleep(2 * time.Second)

	file := filepath.Join(root, "keep.bin")
	attrs, _, aerr := findAttrTag(file)
	if aerr != nil {
		t.Fatalf("findAttrTag: %v", aerr)
	}
	fsout, _ := exec.Command("fsutil", "reparsepoint", "query", file).CombinedOutput()
	truth := "REPARSE PRESENT"
	if !strings.Contains(string(fsout), "Tag value") {
		truth = "NO REPARSE (genuinely plain): " + strings.TrimSpace(string(fsout))
	}
	t.Logf("hard-exit (no Disconnect): disguisedAttrs=0x%x fsutil=%s", attrs, truth)
}
