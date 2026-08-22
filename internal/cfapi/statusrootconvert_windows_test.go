//go:build windows

package cfapi

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// TestStatusRootConvertsRealFile reproduces the laptop's live-mode failure
// (2026-08-19): with a folder registered exactly the way the status-icons
// feature does it — CfRegisterSyncRoot(AlwaysFull/Full) + the brokered WinRT
// shell registration with ShellPolicyStatusOnly — CfConvertToPlaceholder
// failed with 0x8007017C (ERROR_CLOUD_FILE_INVALID_REQUEST) for every one of
// 121k+ files. This test performs that exact registration recipe on a scratch
// dir and converts one real file.
//
// Live-driver test, opt in with NIMBO_CFAPI_LIVE=1.
func TestStatusRootConvertsRealFile(t *testing.T) {
	if os.Getenv("NIMBO_CFAPI_LIVE") == "" {
		t.Skip("set NIMBO_CFAPI_LIVE=1 to run against the real Cloud Files API")
	}
	if !Supported() {
		t.Skip("Cloud Files API not available")
	}
	root := filepath.Join(t.TempDir(), "liveroot")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		UnregisterShellSyncRoot(root)
		_ = UnregisterSyncRoot(root)
	})

	if err := RegisterStatusRoot(root); err != nil {
		t.Fatalf("RegisterStatusRoot: %v", err)
	}
	if err := RegisterShellSyncRoot(root, "NimboStatusTest", "", ShellPolicyStatusOnly); err != nil {
		t.Fatalf("RegisterShellSyncRoot: %v", err)
	}

	file := filepath.Join(root, "doc.txt")
	if err := os.WriteFile(file, []byte("live file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := MarkInSync(file, []byte("doc.txt")); err != nil {
		t.Fatalf("MarkInSync under a status root failed: %v (the laptop's 121k-failure bug)", err)
	}
	attrs, _, err := findAttrTag(file)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("disguised probe: attrs=0x%x", attrs)
	// The laptop's 121k-failure op: a SECOND MarkInSync on the same file, from
	// a process whose probes are disguised (this one). The disguised
	// is-placeholder check says "plain", the blind re-convert used to return
	// 0x8007017C ERROR_CLOUD_FILE_INVALID_REQUEST. It must be idempotent.
	if err := MarkInSync(file, []byte("doc.txt")); err != nil {
		t.Fatalf("second MarkInSync (disguised) failed: %v — the laptop's 121k-failure bug", err)
	}
	out, _ := exec.Command("fsutil", "reparsepoint", "query", file).CombinedOutput()
	if !strings.Contains(string(out), "Tag value") {
		t.Errorf("fsutil: NO reparse — genuinely not converted: %s", strings.TrimSpace(string(out)))
	} else {
		t.Logf("fsutil: REPARSE PRESENT (convert worked; the process probe is disguised)")
	}
	// Can this process opt into the truth on a never-connected status root, or
	// does the exposed view error like it did on a disconnected on-demand root?
	ntdll := windows.NewLazySystemDLL("ntdll.dll")
	_, _, _ = ntdll.NewProc("RtlSetProcessPlaceholderCompatibilityMode").Call(2) // PHCM_EXPOSE_PLACEHOLDERS
	eattrs, _, eerr := findAttrTag(file)
	t.Logf("exposed probe: attrs=0x%x err=%v", eattrs, eerr)
	if b, rerr := os.ReadFile(file); rerr != nil || string(b) != "live file" {
		t.Fatalf("content damaged: %v", rerr)
	}
}
