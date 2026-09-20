//go:build windows

package shellns

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// find returns the value written for key/name, or fails.
func find(t *testing.T, vals []regVal, key, name string) regVal {
	t.Helper()
	for _, v := range vals {
		if v.key == key && v.name == name {
			return v
		}
	}
	t.Fatalf("no value written for %s\\%q", key, name)
	return regVal{}
}

// The shell flags are magic numbers Explorer interprets; getting one wrong
// yields a node that renders but misbehaves (wrong sort slot, not pinned, not
// browsable). Pin them so a refactor can't quietly change one.
func TestDesiredWritesTheShellContract(t *testing.T) {
	vals := desired("Nimbo", `E:\Nextcloud`, `C:\ico\nimbo.ico`)

	if got := find(t, vals, clsidBase, ""); got.s != "Nimbo" {
		t.Errorf("node name = %q, want Nimbo", got.s)
	}
	if got := find(t, vals, clsidBase, "System.IsPinnedToNameSpaceTree"); got.d != 1 {
		t.Errorf("IsPinnedToNameSpaceTree = %d, want 1", got.d)
	}
	if got := find(t, vals, clsidBase, "SortOrderIndex"); got.d != 0x42 {
		t.Errorf("SortOrderIndex = %#x, want 0x42", got.d)
	}
	if got := find(t, vals, clsidBase+`\ShellFolder`, "Attributes"); got.d != 0xF080004D {
		t.Errorf("ShellFolder Attributes = %#x, want 0xF080004D", got.d)
	}
	if got := find(t, vals, clsidBase+`\ShellFolder`, "FolderValueFlags"); got.d != 0x28 {
		t.Errorf("FolderValueFlags = %#x, want 0x28", got.d)
	}
	if got := find(t, vals, clsidBase+`\Instance`, "CLSID"); got.s != delegateCLSID {
		t.Errorf("delegate = %q, want %q", got.s, delegateCLSID)
	}
	if got := find(t, vals, clsidBase+`\Instance\InitPropertyBag`, "Attributes"); got.d != 0x11 {
		t.Errorf("InitPropertyBag Attributes = %#x, want 0x11", got.d)
	}

	// The two that actually vary per install.
	target := find(t, vals, clsidBase+`\Instance\InitPropertyBag`, "TargetFolderPath")
	if target.s != `E:\Nextcloud` {
		t.Errorf("target = %q", target.s)
	}
	if target.kind != kindExpandSz {
		t.Errorf("target kind = %v, want expand-sz", target.kind)
	}
	if got := find(t, vals, clsidBase+`\DefaultIcon`, ""); got.s != `C:\ico\nimbo.ico,0` {
		t.Errorf("icon = %q", got.s)
	}
	// InProcServer32 must stay REG_EXPAND_SZ — it holds %SystemRoot%.
	if got := find(t, vals, clsidBase+`\InProcServer32`, ""); got.kind != kindExpandSz {
		t.Errorf("InProcServer32 kind = %v, want expand-sz", got.kind)
	}
	// The navigation-pane pin and the desktop-icon suppression.
	if got := find(t, vals, nameSpace, ""); got.s != "Nimbo" {
		t.Errorf("namespace pin = %q", got.s)
	}
	if got := find(t, vals, hideDeskKy, NavGUID); got.d != 1 {
		t.Errorf("HideDesktopIcons = %d, want 1", got.d)
	}
}

func TestPsQuoteEscapesEmbeddedQuotes(t *testing.T) {
	cases := map[string]string{
		`C:\Users\Adam`:      `'C:\Users\Adam'`,
		`C:\Users\O'Brien`:   `'C:\Users\O''Brien'`,
		`C:\a$b` + "`c":      "'C:\\a$b`c'", // literal, no PowerShell expansion
		`'; Remove-Item C:\`: `'''; Remove-Item C:\'`,
	}
	for in, want := range cases {
		if got := psQuote(in); got != want {
			t.Errorf("psQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

// A folder or user name holding an apostrophe must not be able to break out of
// the generated script — the path reaches PowerShell as data, never as code.
func TestRegisterScriptQuotesHostilePaths(t *testing.T) {
	evil := `C:\Users\O'Brien'; Remove-Item -Recurse C:\ #`
	s := registerScript(desired("Nimbo", evil, `C:\ico\n.ico`), `C:\ico\n.ico`, []byte("ICO"))
	if strings.Contains(s, "Remove-Item -Recurse C:\\ #\r\n") {
		t.Fatal("hostile path escaped its quoting")
	}
	if !strings.Contains(s, `'C:\Users\O''Brien''; Remove-Item -Recurse C:\ #'`) {
		t.Errorf("path not quoted as a literal:\n%s", s)
	}
}

func TestRegisterScriptWritesIconBeforeRegistering(t *testing.T) {
	icon := `C:\Users\Adam\AppData\Local\Nimbo\nimbo.ico`
	s := registerScript(desired("Nimbo", `E:\Nextcloud`, icon), icon, []byte("ICONBYTES"))

	iconWrite := strings.Index(s, "WriteAllBytes")
	iconValue := strings.Index(s, icon+",0")
	if iconWrite < 0 {
		t.Fatal("script never writes the icon file")
	}
	if iconValue < 0 {
		t.Fatal("script never registers the icon path")
	}
	if iconWrite > iconValue {
		t.Error("DefaultIcon is registered before the icon file exists")
	}
	if want := base64.StdEncoding.EncodeToString([]byte("ICONBYTES")); !strings.Contains(s, "'"+want+"'") {
		t.Errorf("icon bytes not embedded as base64 %q:\n%s", want, s)
	}
	// Types must survive the round trip through PowerShell.
	if !strings.Contains(s, "-PropertyType ExpandString -Force") {
		t.Error("no ExpandString write — TargetFolderPath would land as REG_SZ")
	}
	if !strings.Contains(s, "-PropertyType DWord -Force") {
		t.Error("no DWord write — the shell flags would land as strings")
	}
	// The default value has to be addressed by PowerShell's name for it.
	if !strings.Contains(s, "-Name '(default)'") {
		t.Error("default values not written")
	}
	if !strings.Contains(s, "SHChangeNotify") {
		t.Error("Explorer is never told to reload")
	}
}

func TestUnregisterScriptRemovesEverythingRegisterAdded(t *testing.T) {
	s := unregisterScript()
	for _, want := range []string{`HKCU:\` + clsidBase, `HKCU:\` + nameSpace, `HKCU:\` + hideDeskKy} {
		if !strings.Contains(s, psQuote(want)) {
			t.Errorf("unregister leaves %s behind", want)
		}
	}
	if !strings.Contains(s, "SHChangeNotify") {
		t.Error("Explorer is never told to reload")
	}
}

// The icon must not live in the package data root: that path carries the
// package family name, and a PFN change would blank the navigation-pane icon
// (the same way it blanked the pinned taskbar icons).
func TestStableIconPathAvoidsThePackageDataRoot(t *testing.T) {
	t.Setenv("LOCALAPPDATA", `C:\Users\Adam\AppData\Local`)
	got := stableIconPath("Nimbo")
	if want := filepath.Join(`C:\Users\Adam\AppData\Local`, "Nimbo", "nimbo.ico"); got != want {
		t.Errorf("stableIconPath = %q, want %q", got, want)
	}
	if strings.Contains(strings.ToLower(got), `\packages\`) {
		t.Errorf("icon path is inside the package data root: %s", got)
	}

	t.Setenv("LOCALAPPDATA", "")
	if got := stableIconPath("Nimbo"); got != "" {
		t.Errorf("stableIconPath without LOCALAPPDATA = %q, want empty (caller falls back)", got)
	}
}

func TestUTF16LEBOM(t *testing.T) {
	got := utf16LEBOM("Ab")
	want := []byte{0xFF, 0xFE, 'A', 0x00, 'b', 0x00}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("byte %d = %#x, want %#x", i, got[i], want[i])
		}
	}
}

// A finishing script removes its own files; if two runs shared names, the first
// run's clean-up took the second run's script with it.
func TestScriptPathsAreUniquePerRun(t *testing.T) {
	a1, b1, c1 := scriptPaths(`C:\Users\Adam`)
	a2, b2, c2 := scriptPaths(`C:\Users\Adam`)
	if a1 == a2 || b1 == b2 || c1 == c2 {
		t.Errorf("two runs share a file name: %s %s %s / %s %s %s", a1, b1, c1, a2, b2, c2)
	}
	for _, p := range []string{a1, b1, c1} {
		if filepath.Dir(p) != `C:\Users\Adam` {
			t.Errorf("%s is not under the home dir", p)
		}
	}
}

func TestWaitGone(t *testing.T) {
	p := filepath.Join(t.TempDir(), "run.ps1")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := waitGone(p, 300*time.Millisecond); err == nil {
		t.Error("reported gone while the file still exists")
	}
	go func() { time.Sleep(150 * time.Millisecond); _ = os.Remove(p) }()
	if err := waitGone(p, 5*time.Second); err != nil {
		t.Errorf("not seen going: %v", err)
	}
}
