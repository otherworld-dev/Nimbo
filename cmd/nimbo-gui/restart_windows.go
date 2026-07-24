//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"github.com/otherworld/nimbo/internal/brand"

	"golang.org/x/sys/windows"
)

var (
	kernel32                       = windows.NewLazySystemDLL("kernel32.dll")
	procGetCurrentPackageFamilyName = kernel32.NewProc("GetCurrentPackageFamilyName")
)

// packageFamilyName returns this process's MSIX package family name, or "" when
// running unpackaged (loose exe).
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

// relaunchSelf starts a detached helper that waits ~1s for this process to exit
// (releasing the single-instance lock) and then launches a fresh instance —
// packaged via its AppUserModelID, otherwise the loose exe.
func relaunchSelf() {
	var launch string
	if pfn := packageFamilyName(); pfn != "" {
		launch = `explorer.exe "shell:AppsFolder\` + pfn + `!` + brand.Current.AppID + `"`
	} else if exe, err := os.Executable(); err == nil {
		launch = `start "" "` + exe + `"`
	} else {
		return
	}
	cmd := exec.Command("cmd", "/c", "ping -n 2 127.0.0.1 >nul & "+launch)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS,
	}
	_ = cmd.Start()
}

// canApplyUpdate reports whether an in-app self-update is possible — only when
// running as a packaged (MSIX) app, which is what the App Installer feed updates.
// Never on the Store build: the Store updates the app itself, so self-updating
// from the GitHub feed would fail and would breach Store policy.
func canApplyUpdate() bool { return !isStoreBuild() && packageFamilyName() != "" }

// applyUpdate installs the given versioned .msix asset URL and relaunches. The
// URL must be the release's own immutable asset (…/releases/download/vX/…), NOT
// the latest/download alias or the .appinstaller feed — both can be served
// stale (GitHub CDN / Windows' appinstaller cache), which made updates
// "succeed" while silently reinstalling the current version.
//
// The updater MUST run outside this packaged app's MSIX job object: a plain child
// of the app sits in that job, so when Add-AppxPackage tears the package down to
// install the new version, the job — and the updater mid-install — is killed (the
// "Updating… forever" hang). Three job-escape routes were tried and FAILED from
// inside the packaged process: a temp-file helper (our %TEMP% is the
// package-private AppContainer temp, unreadable by a non-packaged helper); WMI
// Win32_Process.Create (silently blocked from the packaged context); and
// explorer.exe running a .cmd (explorer will launch an app AUMID — what
// relaunchSelf does — but refuses to auto-run a script file when invoked from the
// packaged process). The reliable escape is a SCHEDULED TASK: the Task Scheduler
// service runs it, fully decoupled from our job. We register a one-shot task that
// runs the .ps1 (in the REAL user home, not %TEMP%) and fire it. schtasks errors
// are captured and returned so a failure surfaces instead of hanging; the script
// logs each step to ~/nimbo-update.log and removes its own task when done.
//
// The task is defined via XML (schtasks /create /xml) rather than bare flags
// because schtasks defaults to "start the task only if the computer is on AC
// power" — on a laptop running on battery the triggered task sits in "Queued"
// forever and the update never happens. The XML disables both battery
// conditions.
func applyUpdate(msixURL string) error {
	pfn := packageFamilyName()
	if pfn == "" {
		return errors.New("not a packaged install")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	taskName := brand.Current.AppID + "SelfUpdate"
	ps1 := filepath.Join(home, "nimbo-update.ps1")
	taskXML := filepath.Join(home, "nimbo-update-task.xml")
	logf := filepath.Join(home, "nimbo-update.log")
	// Brand-derived identifiers so a white-label build's updater targets ITS
	// package, not "Nimbo": the MSIX package Name is the pfn up to the first
	// underscore; the AUMID app id is brand.AppID; the exe basename feeds the
	// tray-icon-migration regex (dots escaped for -match).
	pkgName := pfn
	if i := strings.IndexByte(pfn, '_'); i > 0 {
		pkgName = pfn[:i]
	}
	exeRegex := "nimbo-gui\\.exe"
	procName := "nimbo-gui"
	if exe, eerr := os.Executable(); eerr == nil {
		base := filepath.Base(exe)
		exeRegex = strings.ReplaceAll(base, ".", "\\.")
		procName = strings.TrimSuffix(base, filepath.Ext(base))
	}
	// %[1]s msix url, %[2]s pfn, %[3]s log, %[4]s task, %[5]s taskXML,
	// %[6]s package name, %[7]s AUMID app id, %[8]s exe-name regex,
	// %[9]s exe base name without extension (for Get-Process).
	script := fmt.Sprintf(
		"\"$(Get-Date -Format s) helper started: %[1]s\" | Out-File -FilePath '%[3]s' -Append\r\n"+
			// Close the app OURSELVES before installing. Add-AppxPackage's
			// -ForceTargetApplicationShutdown is not free: Windows gives each process
			// in the package container a graceful-close timeout of ~30s before it
			// force-terminates, and that wait lands between "files staged" and
			// "register" — dead time in which the app is already gone from screen.
			// Measured on 0.1.0.137: 13s to download+stage, then a 90.5s terminate gap
			// (3 container processes x ~30s) for a 110s update; a comparable packaged
			// app with a single container process paid exactly 30s. The app
			// accumulates extra container processes between updates (shell/activation
			// launches), so the wait grew with use — the "updates take longer and
			// longer" report. ApplyUpdate quits the app itself before we get here (only
			// the app can stop a tray app), so this is the backstop for whatever it
			// left behind — strays from shell/activation launches. The helper runs in a
			// scheduled task OUTSIDE the container, so it can reach them: ask, wait
			// briefly, then force. Keep that wait SHORT — an external WM_CLOSE cannot
			// quit a tray app, so a long one is spent in full and buys nothing (a 10s
			// deadline cost exactly 10.7s installing 0.1.0.139).
			// -ForceTargetApplicationShutdown stays as a backstop for anything else in
			// the container (e.g. the context-menu COM surrogate); the logged shutdown
			// time plus the deployment's own timing show whether any wait remains.
			"$t0 = Get-Date\r\n"+
			"if (Get-Process -Name '%[9]s' -ErrorAction SilentlyContinue) {\r\n"+
			"  Get-Process -Name '%[9]s' -ErrorAction SilentlyContinue | ForEach-Object { try { $_.CloseMainWindow() | Out-Null } catch {} }\r\n"+
			"  $deadline = (Get-Date).AddSeconds(3)\r\n"+
			"  while ((Get-Date) -lt $deadline -and (Get-Process -Name '%[9]s' -ErrorAction SilentlyContinue)) { Start-Sleep -Milliseconds 250 }\r\n"+
			"  Get-Process -Name '%[9]s' -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue\r\n"+
			"  Start-Sleep -Milliseconds 500\r\n"+
			"  \"$(Get-Date -Format s) app closed in $([int]((Get-Date) - $t0).TotalMilliseconds) ms\" | Out-File -FilePath '%[3]s' -Append\r\n"+
			"}\r\n"+
			// Diagnostic: log every process still holding the package right before we
			// install — one running from the package path (a stray app process) or one
			// that has loaded a package DLL (Explorer / a dllhost COM surrogate holding
			// the shell extension). Add-AppxPackage waits ~30s per such process in its
			// TerminateApplications phase, which is the remaining slow-update cost after
			// the app itself closes fast. This names the culprits in the log so the next
			// slow update is self-diagnosing rather than guesswork.
			"\"$(Get-Date -Format s) package holders before install:\" | Out-File -FilePath '%[3]s' -Append\r\n"+
			"Get-Process -ErrorAction SilentlyContinue | ForEach-Object {\r\n"+
			"  $pp = $_\r\n"+
			"  try {\r\n"+
			"    $hit = $false\r\n"+
			"    if ($pp.Path -like '*WindowsApps\\%[6]s_*') { $hit = $true }\r\n"+
			"    elseif ($pp.Modules | Where-Object { $_.FileName -like '*WindowsApps\\%[6]s_*' }) { $hit = $true }\r\n"+
			"    if ($hit) { \"  $($pp.ProcessName) pid=$($pp.Id)\" | Out-File -FilePath '%[3]s' -Append }\r\n"+
			"  } catch {}\r\n"+
			"}\r\n"+
			"try {\r\n"+
			"  Add-AppxPackage -Path '%[1]s' -ForceTargetApplicationShutdown\r\n"+
			"  \"$(Get-Date -Format s) installed ok, now on $((Get-AppxPackage -Name %[6]s).Version)\" | Out-File -FilePath '%[3]s' -Append\r\n"+
			"} catch {\r\n"+
			"  \"$(Get-Date -Format s) FAILED: $_\" | Out-File -FilePath '%[3]s' -Append\r\n"+
			"}\r\n"+
			// Relaunch via explorer (the same proven route relaunchSelf uses) after a
			// beat for the package registration to settle — Start-Process with a
			// shell: URI from the task context didn't reliably bring the app back.
			"Start-Sleep -Seconds 2\r\n"+
			"explorer.exe 'shell:AppsFolder\\%[2]s!%[7]s'\r\n"+
			// Tray-icon visibility: Windows 11 keys "show by the clock" to the exe
			// PATH, which changes every MSIX version — each update would re-hide the
			// icon. Migrate the choice to the new version's NotifyIconSettings entry
			// (created shortly after relaunch, hence the wait loop) and drop the dead
			// old-version entries. This must run HERE: the app itself is in the MSIX
			// container, whose HKCU writes are virtualized and invisible to Explorer.
			"$nis = 'HKCU:\\Control Panel\\NotifyIconSettings'\r\n"+
			"$ver = (Get-AppxPackage -Name %[6]s).Version\r\n"+
			"$deadline = (Get-Date).AddSeconds(45)\r\n"+
			"while ((Get-Date) -lt $deadline) {\r\n"+
			"  $cur = $null; $stale = @(); $promoted = $false\r\n"+
			"  Get-ChildItem $nis -ErrorAction SilentlyContinue | ForEach-Object {\r\n"+
			"    $p = Get-ItemProperty $_.PSPath\r\n"+
			"    if ($p.ExecutablePath -match '\\\\WindowsApps\\\\%[6]s_.*\\\\%[8]s$') {\r\n"+
			"      if ($p.ExecutablePath -like ('*\\%[6]s_' + $ver + '_*')) { $cur = $_ }\r\n"+
			"      else { $stale += $_; if ($p.IsPromoted -eq 1) { $promoted = $true } }\r\n"+
			"    }\r\n"+
			"  }\r\n"+
			"  if ($cur) {\r\n"+
			"    if ($promoted -and $null -eq (Get-ItemProperty $cur.PSPath).IsPromoted) {\r\n"+
			"      Set-ItemProperty $cur.PSPath -Name IsPromoted -Value 1 -Type DWord\r\n"+
			"      \"$(Get-Date -Format s) tray icon kept visible (migrated to $ver)\" | Out-File -FilePath '%[3]s' -Append\r\n"+
			"    }\r\n"+
			"    $stale | ForEach-Object { Remove-Item $_.PSPath -Recurse -Force -ErrorAction SilentlyContinue }\r\n"+
			"    break\r\n"+
			"  }\r\n"+
			"  Start-Sleep -Seconds 2\r\n"+
			"}\r\n"+
			"schtasks /delete /tn %[4]s /f | Out-Null\r\n"+
			"Remove-Item -LiteralPath '%[5]s' -Force -ErrorAction SilentlyContinue\r\n",
		msixURL, pfn, logf, taskName, taskXML, pkgName, brand.Current.AppID, exeRegex, procName)
	if err := os.WriteFile(ps1, []byte(script), 0o644); err != nil {
		return err
	}
	// The task definition. schtasks' bare /create defaults to "start only on AC
	// power", so the XML explicitly allows battery starts (and survival of an
	// AC→battery flip mid-update). schtasks requires the XML file in UTF-16.
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
    <ExecutionTimeLimit>PT1H</ExecutionTimeLimit>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>powershell</Command>
      <Arguments>-NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -File "%s"</Arguments>
    </Exec>
  </Actions>
</Task>
`, xmlEscape(ps1))
	if err := os.WriteFile(taskXML, utf16LEBOM(xml), 0o644); err != nil {
		return err
	}
	mk := exec.Command("schtasks", "/create", "/tn", taskName, "/xml", taskXML, "/f")
	mk.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if out, err := mk.CombinedOutput(); err != nil {
		return fmt.Errorf("schtasks create failed: %v: %s", err, out)
	}
	run := exec.Command("schtasks", "/run", "/tn", taskName)
	run.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if out, err := run.CombinedOutput(); err != nil {
		return fmt.Errorf("schtasks run failed: %v: %s", err, out)
	}
	return nil
}

// xmlEscape escapes the XML special characters in s for embedding in the task
// definition.
func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

// utf16LEBOM renders s as UTF-16LE bytes with a BOM — the encoding schtasks
// expects for /xml task definitions.
func utf16LEBOM(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2+len(u)*2)
	b[0], b[1] = 0xFF, 0xFE
	for i, r := range u {
		b[2+2*i], b[3+2*i] = byte(r), byte(r>>8)
	}
	return b
}
