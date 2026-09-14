; Inno Setup script for Nimbo's Setup.exe.
;
; It's a bootstrapper, not a traditional file-install: it bundles the signed MSIX
; and, on install (elevated), runs Add-AppxPackage so the real app is the MSIX
; (which owns the Win11 context menu, auto-data container, etc.). The app is
; removed via Settings > Apps, so this installer creates no Program Files app
; dir and no uninstaller of its own.
;
; Every install step lives in [Code] below rather than in bundled PowerShell
; scripts. Hidden PowerShell running a temp .ps1 with -ExecutionPolicy Bypass
; from an elevated installer is the classic dropper shape antivirus heuristics
; key on (Malwarebytes' ML quarantined the v0.1.0.273 Setup.exe, GitHub #5).
; The one step with no native equivalent, Add-AppxPackage, is a single inline
; command of cmdlets, which execution policy doesn't govern - no bypass needed.
;
; Build with build-exe-installer.ps1, which reads the bundled MSIX's identity and
; passes /DAppVer /DFileVer /DPfn /DPkgAppId (plus /DNoDevCert for Azure-signed
; builds). Inputs expected next to this script: Nimbo.msix, NimboDev.cer
; (dev-cert builds only), stage\NCOverlays.dll, stage\icons\*.ico.

#ifndef AppVer
  #define AppVer "0.1.0"
#endif
#ifndef FileVer
  #define FileVer AppVer
#endif
#ifndef Pfn
  #error Pfn is not defined - build with build-exe-installer.ps1, which derives it from Nimbo.msix
#endif
#ifndef PkgAppId
  #define PkgAppId "Nimbo"
#endif
#ifndef Publisher
  #define Publisher "Otherworld Dev Ltd"
#endif
#ifndef FeedUrl
  #define FeedUrl "https://github.com/otherworld-dev/Nimbo/releases/latest/download/Nimbo.appinstaller"
#endif

[Setup]
AppId={{B7E9C1A2-3B4D-4F8A-A1C2-9D5E0F7B6A20}
AppName=Nimbo
AppVersion={#FileVer}
AppPublisher={#Publisher}
AppPublisherURL=https://www.nimbosync.com
AppCopyright=Copyright (C) {#GetDateTimeString('yyyy', '', '')} {#Publisher}
; Version info matching the Authenticode signer: a blank or mismatched company /
; copyright / version is one more "unknown file" signal to reputation scanners.
VersionInfoVersion={#FileVer}
VersionInfoDescription=Nimbo Setup
VersionInfoOriginalFileName=Nimbo-Setup.exe
WizardStyle=modern
PrivilegesRequired=admin
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
CreateAppDir=no
Uninstallable=no
DisableProgramGroupPage=yes
DisableReadyPage=no
OutputDir=.
OutputBaseFilename=Nimbo-Setup-{#AppVer}
Compression=lzma2
SolidCompression=yes
SetupIconFile=..\..\cmd\nimbo-gui\assets\nimbo.ico
AppMutex=Nimbo

[Files]
Source: "Nimbo.msix";       DestDir: "{tmp}"; Flags: deleteafterinstall
; NoDevCert (Azure Trusted Signing builds): the MSIX chains to a public root, so
; no cert is bundled or trusted on the user's machine.
#ifndef NoDevCert
Source: "NimboDev.cer";     DestDir: "{tmp}"; Flags: deleteafterinstall
#endif
; Explorer overlay badges: the icon-overlay DLL cannot be loaded from inside the
; MSIX (Windows refuses to map WindowsApps DLLs into identity-less processes and
; the overlay list is HKLM-only), so Setup - the one elevated moment every
; direct-download install already has - places a copy in Program Files and
; registers it classically, exactly as OneDrive's installer does.
Source: "stage\NCOverlays.dll"; DestDir: "{tmp}"; Flags: deleteafterinstall
Source: "stage\icons\*.ico";    DestDir: "{tmp}\icons"; Flags: deleteafterinstall

[Run]
; Offer to launch Nimbo after install. postinstall entries run as the original
; (non-elevated) user, and explorer.exe on the app's AUMID starts the package
; with its identity.
Filename: "{win}\explorer.exe"; Parameters: "shell:AppsFolder\{#Pfn}!{#PkgAppId}"; \
  Description: "Launch Nimbo now"; Flags: postinstall nowait skipifsilent

[Code]
function SHLoadNonloadedIconOverlayIdentifiers: Integer;
  external 'SHLoadNonloadedIconOverlayIdentifiers@shell32.dll stdcall delayload';

// PSQuote renders S as a PowerShell single-quoted literal (no expansion,
// embedded quotes doubled), so a path can't break out of the inline command.
function PSQuote(const S: String): String;
begin
  Result := S;
  StringChangeEx(Result, '''', '''''', True);
  Result := '''' + Result + '''';
end;

#ifndef NoDevCert
// Dev-cert builds only: a self-signed cert is its own root, so trust it in both
// stores Windows checks.
function TrustDevCert: Boolean;
var
  Cer: String;
  rc: Integer;
begin
  Cer := ExpandConstant('{tmp}\NimboDev.cer');
  Result := False;
  if not Exec(ExpandConstant('{sys}\certutil.exe'), '-f -addstore TrustedPeople "' + Cer + '"', '', SW_HIDE, ewWaitUntilTerminated, rc) then Exit;
  if rc <> 0 then Exit;
  if not Exec(ExpandConstant('{sys}\certutil.exe'), '-f -addstore Root "' + Cer + '"', '', SW_HIDE, ewWaitUntilTerminated, rc) then Exit;
  Result := rc = 0;
end;
#endif

// Closes a running copy, then installs in place (preserves login/config/sync DB).
//
// Prefers the feed: it installs the latest release AND registers App Installer
// update-tracking (a bare-MSIX install gets neither, and a stale bundled MSIX
// would DOWNGRADE a machine that already has a newer build). Falls back to the
// bundled MSIX only when the feed is unreachable; that copy won't auto-update
// until it's next installed online.
function InstallPackage(var rc: Integer): Boolean;
var
  Cmd: String;
begin
  Cmd := '$ErrorActionPreference = ''Stop''; ' +
    'Get-Process -Name nimbo-gui -ErrorAction SilentlyContinue | Stop-Process -Force; ' +
    'try { Add-AppxPackage -AppInstallerFile ' + PSQuote('{#FeedUrl}') + ' -ForceTargetApplicationShutdown } ' +
    'catch { Add-AppxPackage -Path ' + PSQuote(ExpandConstant('{tmp}\Nimbo.msix')) + ' -ForceApplicationShutdown -ForceUpdateFromAnyVersion }';
  Result := Exec('powershell.exe', '-NoProfile -NonInteractive -Command "' + Cmd + '"', '', SW_HIDE, ewWaitUntilTerminated, rc);
end;

// --- Explorer overlay badges (research-settled 2026-08-17, Deck #561) ---
//
// Icon-corner badges are the ONE piece of shell integration an MSIX cannot carry:
// Explorer enumerates HKLM\...\ShellIconOverlayIdentifiers only, refuses in-proc
// activation of packaged COM from identity-less processes, and cannot even map a
// WindowsApps DLL as an image. OneDrive - which declares the same manifest
// element we do - still ships an elevated installer for exactly this step.
//
// So Setup puts a copy of the DLL in Program Files (admin-owned: a user-writable
// DLL loaded into explorer.exe would be a planting vector) and registers it
// classically. Best-effort by design: badges are cosmetic and must never fail
// the install. The Status column works without any of this.
procedure InstallBadges;
var
  Src, ShellDir, Target, IconSrc, IconDir: String;
  Same: Boolean;
  FindRec: TFindRec;
begin
  Src := ExpandConstant('{tmp}\NCOverlays.dll');
  if not FileExists(Src) then Exit;
  try
    ShellDir := ExpandConstant('{commonpf}\Nimbo\Shell');
    ForceDirectories(ShellDir);
    Target := ShellDir + '\NCOverlays.dll';

    Same := False;
    if FileExists(Target) then
      Same := GetSHA256OfFile(Target) = GetSHA256OfFile(Src);
    if not Same then
    begin
      if not CopyFile(Src, Target, False) then
      begin
        // explorer.exe holds the previous copy open. Place the new one in a
        // side dir and re-point the registration; the old copy keeps serving
        // until the next Explorer restart, then goes unused.
        ShellDir := ShellDir + '\v-' + GetDateTimeString('yyyymmddhhnnss', #0, #0);
        ForceDirectories(ShellDir);
        Target := ShellDir + '\NCOverlays.dll';
        if not CopyFile(Src, Target, False) then
          RaiseException('could not place ' + Target);
      end;
    end;

    IconSrc := ExpandConstant('{tmp}\icons');
    IconDir := ShellDir + '\icons';
    ForceDirectories(IconDir);
    if FindFirst(IconSrc + '\*.ico', FindRec) then
    begin
      try
        repeat
          CopyFile(IconSrc + '\' + FindRec.Name, IconDir + '\' + FindRec.Name, False);
        until not FindNext(FindRec);
      finally
        FindClose(FindRec);
      end;
    end;

    // DllRegisterServer writes the CLSIDs and the space-prefixed
    // ShellIconOverlayIdentifiers names using the module's own path - i.e. the
    // Program Files copy just placed. Registered from a 64-bit process.
    RegisterServer(True, Target, True);

    // Ask the running Explorer to pick up newly registered identifiers now.
    // Only works while free overlay slots remain (15 machine-wide); if it does
    // nothing, the next Explorer start loads them.
    SHLoadNonloadedIconOverlayIdentifiers();
  except
    Log('Explorer badges not registered: ' + GetExceptionMessage);
  end;
end;

procedure CurStepChanged(CurStep: TSetupStep);
var
  rc: Integer;
begin
  if CurStep = ssPostInstall then
  begin
    WizardForm.StatusLabel.Caption := 'Installing Nimbo...';
    rc := -1;
#ifndef NoDevCert
    if TrustDevCert then
#endif
    if not InstallPackage(rc) then
      rc := -1;
    Log('Package install returned ' + IntToStr(rc));
    if rc = 0 then
      InstallBadges
    else
      MsgBox('Nimbo could not be installed (step returned ' + IntToStr(rc) + ').' + #13#10 +
             'Try running Setup again, or install Nimbo.msix manually.', mbError, MB_OK);
  end;
end;
