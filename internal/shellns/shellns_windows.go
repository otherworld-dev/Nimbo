//go:build windows

// Package shellns adds (and removes) a Nimbo root in the Windows Explorer
// navigation pane. It uses the documented "delegate folder" namespace pattern —
// a CLSID under HKCU that shell32 hosts and points at a target folder — so it
// needs no COM code and no administrator rights.
//
// PACKAGED (MSIX) BUILDS MUST NOT WRITE THESE KEYS DIRECTLY. Inside the package
// container every HKCU write is virtualized into the package's own private hive
// (…\Packages\<pfn>\SystemAppData\Helium\User.dat): the write reports success
// and the app reads its own value back, but Explorer runs outside the container
// and never sees any of it. That silently broke this feature for every MSIX
// install — the Settings toggle looked like it worked because it was reading
// back its own virtual write, while the navigation pane kept whatever an old
// unpackaged build had left in the real HKCU. It is the same trap the tray-icon
// migration documents in applyUpdate (cmd/nimbo-gui/restart_windows.go).
//
// Packaged builds therefore apply the change from a one-shot SCHEDULED TASK: the
// Task Scheduler service runs it outside our container, so its writes land in
// the real HKCU. Two consequences worth knowing:
//
//   - Reads are unreliable once a build has written virtually. A container read
//     is answered from the package hive first, and deleting a key that exists in
//     the real hive leaves a deletion marker rather than removing it. Callers
//     must remember what they asked for instead of reading it back (see
//     config.Settings.SidebarEnabled / SidebarTarget).
//   - The icon has to live outside the package data root. Everything the app
//     writes under AppData is redirected into …\Packages\<pfn>\LocalCache, whose
//     path changes with the package family name — the same PFN change that
//     blanked the pinned taskbar icons. The task writes the icon to
//     %LOCALAPPDATA%\<brand>\ instead, which survives.
package shellns

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// NavGUID identifies the Nimbo navigation-pane node (fixed for the app).
const NavGUID = "{B7A4C3E0-9D2F-4E18-A6C1-2F5E8B0D7A10}"

// delegateCLSID is shell32's generic folder-shortcut implementation.
const delegateCLSID = "{0E5AAE11-A475-4c5b-AB00-C66DE400274E}"

const (
	clsidBase  = `Software\Classes\CLSID\` + NavGUID
	nameSpace  = `Software\Microsoft\Windows\CurrentVersion\Explorer\Desktop\NameSpace\` + NavGUID
	hideDeskKy = `Software\Microsoft\Windows\CurrentVersion\Explorer\HideDesktopIcons\NewStartPanel`
)

// Supported reports whether the sidebar entry can be configured here.
func Supported() bool { return true }

// Enabled reports whether the Nimbo sidebar node is registered.
//
// Only trustworthy on unpackaged builds. Inside the MSIX container this answers
// from the package's private hive as soon as any build has written there, so a
// packaged caller should prefer its own recorded state and use this only as the
// first-run fallback.
func Enabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, clsidBase, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	_ = k.Close()
	return true
}

// Packaged reports whether this process runs inside an MSIX package, i.e.
// whether registry changes have to be applied out of the container.
func Packaged() bool { return packageFamilyName() != "" }

// --- the values that make up the entry ---

type valKind int

const (
	kindSz valKind = iota
	kindExpandSz
	kindDword
)

// regVal is one HKCU value to write. Name "" means the key's default value.
type regVal struct {
	key  string
	name string
	kind valKind
	s    string
	d    uint32
}

// desired returns every value that defines the sidebar entry, in write order.
// Pure, so both write paths (direct and out-of-container) render the same thing.
func desired(name, targetFolder, iconPath string) []regVal {
	return []regVal{
		{clsidBase, "", kindSz, name, 0},
		{clsidBase, "System.IsPinnedToNameSpaceTree", kindDword, "", 1},
		{clsidBase, "SortOrderIndex", kindDword, "", 0x42},
		{clsidBase + `\DefaultIcon`, "", kindSz, iconPath + ",0", 0},
		{clsidBase + `\InProcServer32`, "", kindExpandSz, `%SystemRoot%\system32\shell32.dll`, 0},
		{clsidBase + `\InProcServer32`, "ThreadingModel", kindSz, "Both", 0},
		{clsidBase + `\Instance`, "CLSID", kindSz, delegateCLSID, 0},
		{clsidBase + `\Instance\InitPropertyBag`, "Attributes", kindDword, "", 0x11},
		{clsidBase + `\Instance\InitPropertyBag`, "TargetFolderPath", kindExpandSz, targetFolder, 0},
		{clsidBase + `\ShellFolder`, "Attributes", kindDword, "", 0xF080004D},
		{clsidBase + `\ShellFolder`, "FolderValueFlags", kindDword, "", 0x28},
		// Pin into the navigation-pane tree and hide the matching Desktop icon.
		{nameSpace, "", kindSz, name, 0},
		{hideDeskKy, NavGUID, kindDword, "", 1},
	}
}

// Register pins a navigation-pane root named name, pointing at targetFolder and
// shown with iconPath. Idempotent; updates target/icon on each call.
//
// On a packaged build the write is handed to a scheduled task and this returns
// once that task has run its script (a second or two), or with an error if it
// did not run within outOfContainerTimeout.
func Register(name, targetFolder, iconPath string) error {
	if Packaged() {
		return registerOutOfContainer(name, targetFolder, iconPath)
	}
	for _, v := range desired(name, targetFolder, iconPath) {
		if err := write(v); err != nil {
			return err
		}
	}
	refresh()
	return nil
}

// Unregister removes the sidebar node.
func Unregister() error {
	if Packaged() {
		return runOutOfContainer("unregister", unregisterScript())
	}
	deleteTree(clsidBase)
	deleteTree(nameSpace)
	delValue(hideDeskKy, NavGUID)
	refresh()
	return nil
}

// --- the node Windows supplies for a cloud sync root ---
//
// A registered cloud sync root (on-demand mode) gets a navigation-pane node
// from Windows itself: a delegate folder under HKCU\Software\Classes\CLSID of
// the same shape as ours, created with the registration and recorded on the
// root's SyncRootManager key as NamespaceCLSID (cfapi.ShellSyncRootNamespaceCLSID).
// The sidebar toggle has to act on that node there — ours is stood down beside
// a sync root so Explorer does not show two Nimbos — and the only handle on its
// visibility is the pinned-to-tree flag, the value that also pins our own entry.
// Verified on a real machine (2026-09-22): clearing it removes the node at once,
// setting it brings the node back, and a restart of the app — which registers
// the same root again — leaves it as set.

// cloudRootPinnedValue is that flag, in Windows' own spelling on the nodes it
// generates (ours writes it as "NameSpace"; registry value names are
// case-insensitive, so either reaches it, but writing it as Windows does keeps
// the key as Windows made it).
const cloudRootPinnedValue = "System.IsPinnedToNamespaceTree"

// cloudRootWait bounds how long a write waits for Windows to finish creating
// the node, which can lag the registration call by a moment.
const cloudRootWait = 10 * time.Second

func cloudRootKey(clsid string) string { return `Software\Classes\CLSID\` + clsid }

// CloudRootPinned reports whether the node clsid is pinned into the navigation
// pane, and whether the node exists at all.
//
// Unlike Enabled, this read is trustworthy inside the MSIX container: the key
// is only ever written outside it (by Windows at registration, or by our
// scheduled task), so there is no private copy to shadow the real one.
func CloudRootPinned(clsid string) (pinned, exists bool) {
	k, err := registry.OpenKey(registry.CURRENT_USER, cloudRootKey(clsid), registry.QUERY_VALUE)
	if err != nil {
		return false, false
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue(cloudRootPinnedValue)
	if err != nil {
		return false, true
	}
	return v == 1, true
}

// SetCloudRootPinned shows (on) or hides the node Windows supplies for a cloud
// sync root. The node is Windows' own: only its flag is written, never the key,
// so a node that does not exist is an error rather than something to invent.
// On a packaged build the write goes out of the container, like Register's.
func SetCloudRootPinned(clsid string, on bool) error {
	if Packaged() {
		action := "hide-cloud-root"
		if on {
			action = "show-cloud-root"
		}
		return runOutOfContainer(action, cloudRootPinScript(clsid, on))
	}
	var v uint32
	if on {
		v = 1
	}
	var k registry.Key
	var err error
	for deadline := time.Now().Add(cloudRootWait); ; {
		k, err = registry.OpenKey(registry.CURRENT_USER, cloudRootKey(clsid), registry.SET_VALUE)
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("navigation-pane node %s: %w", clsid, err)
	}
	defer k.Close()
	if err := k.SetDWordValue(cloudRootPinnedValue, v); err != nil {
		return err
	}
	refresh()
	return nil
}

// cloudRootPinScript renders the out-of-container write: wait briefly for the
// node, set its flag, tell Explorer. It never creates the key.
func cloudRootPinScript(clsid string, on bool) string {
	var v int
	if on {
		v = 1
	}
	var b strings.Builder
	b.WriteString("$ErrorActionPreference = 'Continue'\r\n")
	b.WriteString(fmt.Sprintf("$k = %s\r\n", psQuote(`HKCU:\`+cloudRootKey(clsid))))
	b.WriteString(fmt.Sprintf("$deadline = (Get-Date).AddSeconds(%d)\r\n", int(cloudRootWait/time.Second)))
	b.WriteString("while (-not (Test-Path -LiteralPath $k) -and (Get-Date) -lt $deadline) { Start-Sleep -Milliseconds 250 }\r\n")
	b.WriteString(fmt.Sprintf("if (Test-Path -LiteralPath $k) { New-ItemProperty -LiteralPath $k -Name %s -Value %d -PropertyType DWord -Force | Out-Null }\r\n",
		psQuote(cloudRootPinnedValue), v))
	b.WriteString(notifyShellPS)
	return b.String()
}

// --- out-of-container application (packaged builds) ---

// stableIconPath is where a packaged build's sidebar icon lives: under
// %LOCALAPPDATA%\<brand>\, NOT in the package data root, so that a package
// family name change does not blank the navigation-pane icon. Only the
// scheduled task can create it — the app's own writes there are redirected —
// but the path itself resolves correctly from inside the container.
func stableIconPath(brandDir string) string {
	local := os.Getenv("LOCALAPPDATA")
	if local == "" || brandDir == "" {
		return ""
	}
	return filepath.Join(local, brandDir, "nimbo.ico")
}

// registerOutOfContainer renders the registration as a PowerShell script and
// runs it from a scheduled task. The icon travels as base64 inside the script:
// the app can read its own copy but the task cannot (the app's path is
// redirected into the package's LocalCache), and hard-coding that redirected
// path would tie the icon to the current package family name.
func registerOutOfContainer(name, targetFolder, iconPath string) error {
	icon := stableIconPath(name)
	if icon == "" {
		icon = iconPath
	}
	ico, err := os.ReadFile(iconPath)
	if err != nil {
		return fmt.Errorf("read sidebar icon: %w", err)
	}
	return runOutOfContainer("register", registerScript(desired(name, targetFolder, icon), icon, ico))
}

// registerScript renders the values as PowerShell. The icon bytes are written
// first so the DefaultIcon value never points at a file that is not there yet.
func registerScript(vals []regVal, iconPath string, ico []byte) string {
	var b strings.Builder
	b.WriteString("$ErrorActionPreference = 'Continue'\r\n")
	b.WriteString(fmt.Sprintf("$icon = %s\r\n", psQuote(iconPath)))
	// .NET rather than New-Item: New-Item has no -LiteralPath, and a brand whose
	// name contains [ ] would be read as a wildcard.
	b.WriteString("[IO.Directory]::CreateDirectory((Split-Path -Parent $icon)) | Out-Null\r\n")
	b.WriteString(fmt.Sprintf("[IO.File]::WriteAllBytes($icon, [Convert]::FromBase64String('%s'))\r\n",
		base64.StdEncoding.EncodeToString(ico)))
	for _, v := range vals {
		key := `HKCU:\` + v.key
		b.WriteString(fmt.Sprintf("if (-not (Test-Path -LiteralPath %s)) { New-Item -Path %s -Force | Out-Null }\r\n",
			psQuote(key), psQuote(key)))
		nm := v.name
		if nm == "" {
			nm = "(default)"
		}
		switch v.kind {
		case kindDword:
			b.WriteString(fmt.Sprintf("New-ItemProperty -LiteralPath %s -Name %s -Value %d -PropertyType DWord -Force | Out-Null\r\n",
				psQuote(key), psQuote(nm), v.d))
		case kindExpandSz:
			b.WriteString(fmt.Sprintf("New-ItemProperty -LiteralPath %s -Name %s -Value %s -PropertyType ExpandString -Force | Out-Null\r\n",
				psQuote(key), psQuote(nm), psQuote(v.s)))
		default:
			b.WriteString(fmt.Sprintf("New-ItemProperty -LiteralPath %s -Name %s -Value %s -PropertyType String -Force | Out-Null\r\n",
				psQuote(key), psQuote(nm), psQuote(v.s)))
		}
	}
	b.WriteString(notifyShellPS)
	return b.String()
}

// unregisterScript removes the entry from the real HKCU.
func unregisterScript() string {
	var b strings.Builder
	b.WriteString("$ErrorActionPreference = 'Continue'\r\n")
	for _, k := range []string{clsidBase, nameSpace} {
		b.WriteString(fmt.Sprintf("Remove-Item -LiteralPath %s -Recurse -Force -ErrorAction SilentlyContinue\r\n",
			psQuote(`HKCU:\`+k)))
	}
	b.WriteString(fmt.Sprintf("Remove-ItemProperty -LiteralPath %s -Name %s -Force -ErrorAction SilentlyContinue\r\n",
		psQuote(`HKCU:\`+hideDeskKy), psQuote(NavGUID)))
	b.WriteString(notifyShellPS)
	return b.String()
}

// notifyShellPS makes Explorer reload its namespace. SHChangeNotify has to be
// called from out here too: a broadcast raised inside the container refers to a
// change Explorer cannot see.
const notifyShellPS = "Add-Type -MemberDefinition '[DllImport(\"shell32.dll\")] public static extern void SHChangeNotify(int e, uint f, IntPtr a, IntPtr b);' -Name ShNs -Namespace NimboSidebar -ErrorAction SilentlyContinue\r\n" +
	"try { [NimboSidebar.ShNs]::SHChangeNotify(0x08000000, 0, [IntPtr]::Zero, [IntPtr]::Zero) } catch {}\r\n"

// outOfContainerMu serialises out-of-container runs. Each run holds it until
// its script has finished, so two toggles in quick succession apply in order
// instead of racing each other in the registry, and a caller that records the
// preference after Register/Unregister returns is recording a change that has
// really been made.
var outOfContainerMu sync.Mutex

// outOfContainerTimeout bounds the wait for the scheduled task to run its
// script. Normally that takes a second or two; the bound only matters when the
// Task Scheduler service is stopped or the task never starts, where the run is
// reported as failed and its leftovers are cleared so the next attempt starts
// clean.
const outOfContainerTimeout = 30 * time.Second

// runOutOfContainer writes body to a .ps1 and runs it via a one-shot scheduled
// task, which executes outside our MSIX job so its HKCU writes are real. It
// returns once the script has run to its end.
//
// The script and its task definition go in the REAL user home, not %TEMP%: our
// temp directory is the package-private AppContainer one and the task, running
// unpackaged, cannot read it. The task is defined by XML rather than bare
// schtasks flags because schtasks defaults to "only on AC power", which leaves
// the task Queued forever on a laptop on battery. Both are the same constraints
// applyUpdate documents.
//
// Every run gets its own file names and runs alone (outOfContainerMu). With
// shared names and no wait, a second toggle inside the first one's second or
// so either replaced the first script before the scheduler had opened it (the
// first request silently never ran) or had its own task XML deleted by the
// first script's clean-up before schtasks read it ("schtasks create failed:
// … The system cannot find the file specified").
func runOutOfContainer(action, body string) error {
	outOfContainerMu.Lock()
	defer outOfContainerMu.Unlock()

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	pkg := packageName()
	if pkg == "" {
		pkg = "Nimbo"
	}
	taskName := pkg + "SidebarUpdate"
	ps1, vbs, taskXML := scriptPaths(home)
	logf := filepath.Join(home, "nimbo-sidebar.log")

	// The script deletes itself last, so its disappearance is the signal that
	// everything before it — the body included — has run.
	script := fmt.Sprintf("\"$(Get-Date -Format s) sidebar %s\" | Out-File -FilePath %s -Append\r\n", action, psQuote(logf)) +
		body +
		fmt.Sprintf("\"$(Get-Date -Format s) sidebar %s done\" | Out-File -FilePath %s -Append\r\n", action, psQuote(logf)) +
		fmt.Sprintf("schtasks /delete /tn %s /f | Out-Null\r\n", taskName) +
		fmt.Sprintf("Remove-Item -LiteralPath %s -Force -ErrorAction SilentlyContinue\r\n", psQuote(taskXML)) +
		fmt.Sprintf("Remove-Item -LiteralPath %s -Force -ErrorAction SilentlyContinue\r\n", psQuote(vbs)) +
		"Remove-Item -LiteralPath $PSCommandPath -Force -ErrorAction SilentlyContinue\r\n"
	if err := os.WriteFile(ps1, []byte(script), 0o644); err != nil {
		return err
	}
	// The task cannot run powershell.exe directly: a console process gets its
	// conhost window created BEFORE -WindowStyle Hidden is processed, so every
	// sync-mode switch flashed a black box. wscript.exe is a windowless host,
	// and Run's window style 0 creates the console hidden from the start.
	launcher := "CreateObject(\"WScript.Shell\").Run \"powershell -NoProfile -ExecutionPolicy Bypass -File \"\"" +
		ps1 + "\"\"\", 0, False\r\n"
	if err := os.WriteFile(vbs, []byte(launcher), 0o644); err != nil {
		return err
	}
	xml := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <Principals>
    <Principal id="Author">
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <StartWhenAvailable>true</StartWhenAvailable>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <ExecutionTimeLimit>PT10M</ExecutionTimeLimit>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>wscript.exe</Command>
      <Arguments>//B //Nologo "%s"</Arguments>
    </Exec>
  </Actions>
</Task>
`, xmlEscape(vbs))
	if err := os.WriteFile(taskXML, utf16LEBOM(xml), 0o644); err != nil {
		return err
	}
	if out, err := schtasks("/create", "/tn", taskName, "/xml", taskXML, "/f"); err != nil {
		return fmt.Errorf("schtasks create failed: %v: %s", err, out)
	}
	if out, err := schtasks("/run", "/tn", taskName); err != nil {
		return fmt.Errorf("schtasks run failed: %v: %s", err, out)
	}
	if err := waitGone(ps1, outOfContainerTimeout); err != nil {
		// It never ran (or is stuck): stop it and clear up, so the next attempt
		// is not blocked by a task the scheduler still considers running.
		_, _ = schtasks("/end", "/tn", taskName)
		_, _ = schtasks("/delete", "/tn", taskName, "/f")
		for _, f := range []string{taskXML, vbs, ps1} {
			_ = os.Remove(f)
		}
		return fmt.Errorf("the Explorer change did not apply: %w", err)
	}
	return nil
}

// runSeq numbers this process's runs; the wall clock alone is too coarse on
// Windows to tell two back-to-back runs apart.
var runSeq atomic.Uint64

// scriptPaths returns fresh names for one run's script, launcher and task
// definition under home. They are unique per run because a finishing script
// removes its own files, which with shared names took the next run's with them.
func scriptPaths(home string) (ps1, vbs, taskXML string) {
	stem := fmt.Sprintf("nimbo-sidebar-%d-%d-%d", os.Getpid(), time.Now().Unix(), runSeq.Add(1))
	return filepath.Join(home, stem+".ps1"),
		filepath.Join(home, stem+".vbs"),
		filepath.Join(home, stem+"-task.xml")
}

// schtasks runs one schtasks.exe command with its console hidden.
func schtasks(args ...string) ([]byte, error) {
	c := exec.Command("schtasks", args...)
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return c.CombinedOutput()
}

// waitGone polls until path no longer exists, or fails once timeout has passed.
func waitGone(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for the scheduled task to run", timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// psQuote renders s as a PowerShell single-quoted literal (no expansion, so a
// path holding a $ or a backtick stays literal; an embedded quote is doubled).
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// xmlEscape escapes the XML special characters in s for the task definition.
func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

// utf16LEBOM renders s as UTF-16LE bytes with a BOM — the encoding schtasks
// expects for /xml task definitions.
func utf16LEBOM(s string) []byte {
	u := windows.StringToUTF16(s)
	b := []byte{0xFF, 0xFE}
	for _, c := range u[:len(u)-1] { // drop the NUL terminator
		b = append(b, byte(c), byte(c>>8))
	}
	return b
}

// --- package identity ---

var (
	kernel32                        = windows.NewLazySystemDLL("kernel32.dll")
	procGetCurrentPackageFamilyName = kernel32.NewProc("GetCurrentPackageFamilyName")
)

// packageFamilyName returns this process's MSIX package family name, or "" when
// the build is not packaged.
func packageFamilyName() string {
	var length uint32
	procGetCurrentPackageFamilyName.Call(uintptr(unsafe.Pointer(&length)), 0)
	if length == 0 {
		return "" // APPMODEL_ERROR_NO_PACKAGE — not packaged
	}
	buf := make([]uint16, length)
	r, _, _ := procGetCurrentPackageFamilyName.Call(uintptr(unsafe.Pointer(&length)), uintptr(unsafe.Pointer(&buf[0])))
	if r != 0 {
		return ""
	}
	return windows.UTF16ToString(buf)
}

// packageName is the MSIX package Name — the family name up to the first
// underscore. Used to name the task after the running brand's own package so a
// white-label build does not collide with Nimbo's.
func packageName() string {
	pfn := packageFamilyName()
	if i := strings.IndexByte(pfn, '_'); i > 0 {
		return pfn[:i]
	}
	return pfn
}

// --- registry helpers (direct path, unpackaged builds) ---

func write(v regVal) error {
	switch v.kind {
	case kindDword:
		return setDword(v.key, v.name, v.d)
	case kindExpandSz:
		return setExpand(v.key, v.name, v.s)
	default:
		return setStr(v.key, v.name, v.s)
	}
}

func setStr(path, name, val string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, path, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(name, val)
}

func setExpand(path, name, val string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, path, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetExpandStringValue(name, val)
}

func setDword(path, name string, val uint32) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, path, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetDWordValue(name, val)
}

func delValue(path, name string) {
	k, err := registry.OpenKey(registry.CURRENT_USER, path, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	_ = k.DeleteValue(name)
}

func deleteTree(path string) {
	if k, err := registry.OpenKey(registry.CURRENT_USER, path, registry.READ); err == nil {
		subs, _ := k.ReadSubKeyNames(-1)
		_ = k.Close()
		for _, s := range subs {
			deleteTree(path + `\` + s)
		}
	}
	_ = registry.DeleteKey(registry.CURRENT_USER, path)
}

var (
	shell32            = windows.NewLazySystemDLL("shell32.dll")
	procSHChangeNotify = shell32.NewProc("SHChangeNotify")
)

// refresh tells Explorer to reload its namespace so the change is visible.
func refresh() {
	const SHCNE_ASSOCCHANGED = 0x08000000
	const SHCNF_IDLIST = 0x0000
	_, _, _ = procSHChangeNotify.Call(uintptr(SHCNE_ASSOCCHANGED), uintptr(SHCNF_IDLIST), 0, 0)
}

// --- sidebar entries left behind (GitHub #10) ---

// NavNodes lists the sidebar folder entries in HKCU that carry an icon, other
// than Nimbo's own entry (NavGUID). Windows creates one per registered sync
// root, and has been seen to leave them behind after the root itself is gone;
// the caller decides which are its own by their icon. Reads are safe from
// inside the MSIX container for keys the app never wrote, which these are
// (Windows writes them).
func NavNodes() []NavNode {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Classes\CLSID`, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return nil
	}
	names, _ := k.ReadSubKeyNames(-1)
	_ = k.Close()
	var out []NavNode
	for _, c := range names {
		if strings.EqualFold(c, NavGUID) {
			continue
		}
		bag, err := registry.OpenKey(registry.CURRENT_USER, `Software\Classes\CLSID\`+c+`\Instance\InitPropertyBag`, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		target, _, _ := bag.GetStringValue("TargetFolderPath")
		_ = bag.Close()
		if target == "" {
			continue
		}
		icon := ""
		if ik, err := registry.OpenKey(registry.CURRENT_USER, `Software\Classes\CLSID\`+c+`\DefaultIcon`, registry.QUERY_VALUE); err == nil {
			icon, _, _ = ik.GetStringValue("")
			_ = ik.Close()
		}
		if icon == "" {
			continue
		}
		out = append(out, NavNode{CLSID: c, Target: target, Icon: icon})
	}
	return out
}

// RemoveNavNodes deletes the given sidebar entries. On a packaged build the
// change is made out of the container, as for Nimbo's own entry, or Explorer
// would never see it.
func RemoveNavNodes(clsids []string) error {
	if len(clsids) == 0 {
		return nil
	}
	if Packaged() {
		return runOutOfContainer("remove-leftovers", removeNodesScript(clsids))
	}
	for _, c := range clsids {
		deleteTree(`Software\Classes\CLSID\` + c)
		deleteTree(`Software\Microsoft\Windows\CurrentVersion\Explorer\Desktop\NameSpace\` + c)
		delValue(hideDeskKy, c)
	}
	refresh()
	return nil
}

// removeNodesScript renders the PowerShell that deletes each entry's CLSID
// key, its Desktop\NameSpace pin and its hidden-desktop-icon value.
func removeNodesScript(clsids []string) string {
	var b strings.Builder
	b.WriteString("$ErrorActionPreference = 'Continue'\r\n")
	for _, c := range clsids {
		for _, k := range []string{`Software\Classes\CLSID\` + c, `Software\Microsoft\Windows\CurrentVersion\Explorer\Desktop\NameSpace\` + c} {
			b.WriteString(fmt.Sprintf("Remove-Item -LiteralPath %s -Recurse -Force -ErrorAction SilentlyContinue\r\n", psQuote(`HKCU:\`+k)))
		}
		b.WriteString(fmt.Sprintf("Remove-ItemProperty -LiteralPath %s -Name %s -Force -ErrorAction SilentlyContinue\r\n",
			psQuote(`HKCU:\`+hideDeskKy), psQuote(c)))
	}
	b.WriteString(notifyShellPS)
	return b.String()
}
