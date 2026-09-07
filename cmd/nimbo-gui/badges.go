package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/otherworld/nimbo/internal/config"
	"github.com/otherworld/nimbo/internal/notify"
)

// In-app, opt-in registration of the Explorer corner badges — the one piece of
// shell integration an MSIX cannot carry itself (HKLM-only identifier list;
// see setup-steps.ps1, which does the same job during a direct-download
// install's elevated moment). Store and in-app-updated installs have no
// elevated moment, so the user triggers one here: a single UAC consent runs a
// one-shot elevated PowerShell that copies the signed DLL to Program Files
// (admin-owned — a user-writable DLL loaded into explorer.exe would be a
// planting vector) and registers it classically.
//
// The elevated child is launched through the AppInfo service (ShellExecuteEx
// "runas"), which is what lets it escape the package container; the result is
// VERIFIED against the real registry afterwards rather than assumed.

const overlayIdentRoot = `SOFTWARE\Microsoft\Windows\CurrentVersion\Explorer\ShellIconOverlayIdentifiers`

// badgePrioritySpaces mirrors kPrioritySpaces in overlays.cpp — the current
// generation of the registration. Older installs registered at 18 spaces,
// which OneDrive's 19-space entries out-prioritise into never painting; those
// count as "not registered" so the user is offered the fix.
const badgePrioritySpaces = 20

// badgesRegistered reports whether the CURRENT badge generation is registered:
// all four identifiers present at the current priority, and the registered
// DLL still on disk.
func badgesRegistered() bool {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, overlayIdentRoot, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return false
	}
	defer k.Close()
	names, err := k.ReadSubKeyNames(-1)
	if err != nil {
		return false
	}
	want := map[string]bool{"Nimbo1Synced": false, "Nimbo2Syncing": false, "Nimbo3Warning": false, "Nimbo4Shared": false}
	for _, n := range names {
		trimmed := strings.TrimLeft(n, " ")
		if _, ours := want[trimmed]; ours && len(n)-len(trimmed) == badgePrioritySpaces {
			want[trimmed] = true
		}
	}
	for _, present := range want {
		if !present {
			return false
		}
	}
	ck, err := registry.OpenKey(registry.CLASSES_ROOT,
		`CLSID\{4F8B2C10-1A3D-4E55-9B21-0C7E5A9D0004}\InprocServer32`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer ck.Close()
	dll, _, err := ck.GetStringValue("")
	if err != nil || dll == "" {
		return false
	}
	_, err = os.Stat(dll)
	return err == nil
}

// enableBadges stages the package's overlay DLL and icons into the user
// profile, then runs the elevated registration step behind one UAC consent.
// Blocks until the helper finishes (or the user declines) and reports the
// verified outcome.
func (a *App) enableBadges() string {
	exe, err := os.Executable()
	if err != nil {
		return err.Error()
	}
	pkgDir := filepath.Dir(exe)
	srcDll := filepath.Join(pkgDir, "NCOverlays.dll")
	// Read the DLL from the package directory (admin-owned WindowsApps, which
	// the elevated helper cannot itself read) and pin it by SHA-256 computed
	// HERE. Everything the elevated helper touches is user-writable staging, so
	// it trusts nothing on disk: it re-hashes the staged DLL against this value
	// and checks its Authenticode signature before copying or registering it.
	// That closes the TOCTOU by which local malware could swap the staged DLL
	// for its own and have regsvr32 load it elevated + machine-wide.
	dllBytes, err := os.ReadFile(srcDll)
	if err != nil {
		return "the badge component is missing from this install"
	}
	sum := sha256.Sum256(dllBytes)
	wantHash := strings.ToUpper(hex.EncodeToString(sum[:]))

	home, err := os.UserHomeDir()
	if err != nil {
		return err.Error()
	}
	// Freshly created, randomly-named staging dir — unpredictable to a would-be
	// pre-planter, though the hash/signature check below is the real defence.
	stage, err := os.MkdirTemp(home, "nimbo-badges-")
	if err != nil {
		return err.Error()
	}
	defer os.RemoveAll(stage)
	if err := os.MkdirAll(filepath.Join(stage, "icons"), 0o755); err != nil {
		return err.Error()
	}
	if err := os.WriteFile(filepath.Join(stage, "NCOverlays.dll"), dllBytes, 0o644); err != nil {
		return "couldn't stage the badge component: " + err.Error()
	}
	// Icons are data (loaded by the verified DLL via GetOverlayInfo), not code,
	// so they need no signature gate.
	if icons, err := filepath.Glob(filepath.Join(pkgDir, "icons", "*.ico")); err == nil {
		for _, ico := range icons {
			if b, e := os.ReadFile(ico); e == nil {
				_ = os.WriteFile(filepath.Join(stage, "icons", filepath.Base(ico)), b, 0o644)
			}
		}
	}
	result := filepath.Join(stage, "result.txt")

	// The elevated step is passed INLINE via -EncodedCommand: no script file on
	// disk for a non-admin to tamper with between write and elevated read. It
	// verifies the staged DLL (hash pin + Authenticode) before trusting it,
	// then mirrors setup-steps.ps1 (side-dir upgrade when Explorer holds the
	// old DLL open).
	body := `$ErrorActionPreference = 'Stop'
$stage = ` + psSingleQuote(stage) + `
$want = ` + psSingleQuote(wantHash) + `
$result = ` + psSingleQuote(result) + `
$src = Join-Path $stage 'NCOverlays.dll'
try {
  $h = (Get-FileHash -Algorithm SHA256 -LiteralPath $src).Hash.ToUpper()
  if ($h -ne $want) { throw 'staged component failed its integrity check' }
  $sig = Get-AuthenticodeSignature -LiteralPath $src
  if ($sig.Status -ne 'Valid') { throw "staged component is not validly signed ($($sig.Status))" }
  if ($sig.SignerCertificate.Subject -notlike '*Otherworld Dev*') { throw 'staged component has an unexpected signer' }
  $shellDir = Join-Path $env:ProgramFiles 'Nimbo\Shell'
  New-Item -ItemType Directory -Force -Path $shellDir | Out-Null
  $target = Join-Path $shellDir 'NCOverlays.dll'
  try {
    Copy-Item -Force -LiteralPath $src $target -ErrorAction Stop
  } catch {
    $side = Join-Path $shellDir ('v-' + [guid]::NewGuid().ToString('N').Substring(0, 8))
    New-Item -ItemType Directory -Force -Path $side | Out-Null
    $target = Join-Path $side 'NCOverlays.dll'
    Copy-Item -Force -LiteralPath $src $target
  }
  $icoDir = Join-Path (Split-Path $target -Parent) 'icons'
  New-Item -ItemType Directory -Force -Path $icoDir | Out-Null
  Copy-Item -Force (Join-Path $stage 'icons\*.ico') $icoDir
  & "$env:SystemRoot\System32\regsvr32.exe" /s $target
  Add-Type -Namespace NimboBadges -Name Shell -MemberDefinition '[DllImport("shell32.dll")] public static extern int SHLoadNonloadedIconOverlayIdentifiers();'
  [NimboBadges.Shell]::SHLoadNonloadedIconOverlayIdentifiers() | Out-Null
  'ok' | Out-File -LiteralPath $result -Encoding ascii
} catch {
  ('error: ' + $_) | Out-File -LiteralPath $result -Encoding ascii
}
`
	args := `-NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -EncodedCommand ` + encodePSCommand(body)
	if msg := runElevatedAndWait("powershell.exe", args, 2*time.Minute); msg != "" {
		return msg
	}

	// Trust the registry, not the helper: the outcome must be visible in the
	// REAL HKLM (an elevated child that kept package identity would have had
	// its writes virtualised away).
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if badgesRegistered() {
			slog.Info("explorer badges registered via in-app elevation")
			notify.Toast("Folder badges enabled", "Sync badges will appear on your files — restart Explorer if they don't show yet.", "")
			return ""
		}
		time.Sleep(2 * time.Second)
	}
	if b, err := os.ReadFile(result); err == nil && strings.HasPrefix(string(b), "error:") {
		return "Windows couldn't register the badges: " + strings.TrimSpace(strings.TrimPrefix(string(b), "error:"))
	}
	return "the badge registration didn't take — try again, or use the full installer"
}

// psSingleQuote renders s as a PowerShell single-quoted literal — no expansion,
// embedded quotes doubled — so a staging path can never break out of the string
// or inject into the elevated command.
func psSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// encodePSCommand base64-encodes a script as UTF-16LE for powershell.exe's
// -EncodedCommand, so the whole elevated script lives on the (tamper-proof
// once launched) command line rather than a file on disk.
func encodePSCommand(s string) string {
	u16 := utf16.Encode([]rune(s))
	buf := make([]byte, len(u16)*2)
	for i, r := range u16 {
		binary.LittleEndian.PutUint16(buf[i*2:], r)
	}
	return base64.StdEncoding.EncodeToString(buf)
}

// maybeOfferBadges raises a one-time actionable toast after a switch to live
// mode when the current badge generation is not registered — Adam's design:
// "when lfs is selected, a message pops up asking the user if they want to
// enable the badges". The Settings button remains the permanent home.
func (a *App) maybeOfferBadges() {
	if badgesRegistered() {
		return
	}
	d, err := config.Resolve()
	if err != nil {
		return
	}
	already := false
	_ = d.UpdateSettings(func(s *config.Settings) {
		already = s.BadgesOffered
		s.BadgesOffered = true
	})
	if already {
		return
	}
	notify.RaiseActionable("Show sync badges on your files?",
		"Windows will ask for permission once, then your synced folders get their status icons.",
		"action=badges",
		[]notify.ToastButton{{Label: "Enable badges", Args: "action=badges"}})
}

// runElevatedAndWait launches file through the UAC consent flow (AppInfo
// service, which is what breaks out of the MSIX container) and waits for it
// to finish. Returns "" on a completed run, or a user-facing message.
func runElevatedAndWait(file, args string, timeout time.Duration) string {
	verb, _ := windows.UTF16PtrFromString("runas")
	f, _ := windows.UTF16PtrFromString(file)
	p, _ := windows.UTF16PtrFromString(args)
	sei := struct {
		cbSize         uint32
		fMask          uint32
		hwnd           windows.Handle
		lpVerb         *uint16
		lpFile         *uint16
		lpParameters   *uint16
		lpDirectory    *uint16
		nShow          int32
		hInstApp       windows.Handle
		lpIDList       uintptr
		lpClass        *uint16
		hkeyClass      windows.Handle
		dwHotKey       uint32
		hIconOrMonitor windows.Handle
		hProcess       windows.Handle
	}{
		fMask:        0x00000040 | 0x00000100, // SEE_MASK_NOCLOSEPROCESS | SEE_MASK_NO_CONSOLE
		lpVerb:       verb,
		lpFile:       f,
		lpParameters: p,
		nShow:        0, // SW_HIDE
	}
	sei.cbSize = uint32(unsafe.Sizeof(sei))
	shell32 := windows.NewLazySystemDLL("shell32.dll")
	proc := shell32.NewProc("ShellExecuteExW")
	r, _, callErr := proc.Call(uintptr(unsafe.Pointer(&sei)))
	if r == 0 {
		if errno, ok := callErr.(windows.Errno); ok && errno == windows.ERROR_CANCELLED {
			return "Windows permission was declined — badges stay off"
		}
		return fmt.Sprintf("couldn't request permission: %v", callErr)
	}
	if sei.hProcess == 0 {
		return "" // launched but not waitable; the verification loop decides
	}
	defer windows.CloseHandle(sei.hProcess)
	ev, err := windows.WaitForSingleObject(sei.hProcess, uint32(timeout.Milliseconds()))
	if err != nil || ev != windows.WAIT_OBJECT_0 {
		return "" // slow or unknown; the verification loop decides
	}
	return ""
}
