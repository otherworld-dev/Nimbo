; Inno Setup script for Nimbo's Setup.exe.
;
; It's a bootstrapper, not a traditional file-install: it bundles the signed MSIX
; + signing cert, and on install (elevated) trusts the cert and runs Add-AppxPackage
; so the real app is the MSIX (which owns the Win11 context menu, auto-data
; container, etc.). The app is removed via Settings > Apps, so this installer
; creates no Program Files dir and no uninstaller of its own.
;
; Build with build-exe-installer.ps1 (passes /DAppVer). Inputs expected next to
; this script: Nimbo.msix, NimboDev.cer, setup-steps.ps1.

#ifndef AppVer
  #define AppVer "0.1.0"
#endif

[Setup]
AppId={{B7E9C1A2-3B4D-4F8A-A1C2-9D5E0F7B6A20}
AppName=Nimbo
AppVersion={#AppVer}
AppPublisher=Nimbo
AppPublisherURL=https://www.nimbosync.com
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
Source: "setup-steps.ps1";  DestDir: "{tmp}"; Flags: deleteafterinstall
Source: "launch-nimbo.ps1"; DestDir: "{tmp}"; Flags: deleteafterinstall

[Run]
; Offer to launch Nimbo after install.
Filename: "powershell.exe"; \
  Parameters: "-NoProfile -ExecutionPolicy Bypass -File ""{tmp}\launch-nimbo.ps1"""; \
  Description: "Launch Nimbo now"; Flags: postinstall nowait runhidden skipifsilent

[Code]
procedure CurStepChanged(CurStep: TSetupStep);
var
  rc: Integer;
begin
  if CurStep = ssPostInstall then
  begin
    WizardForm.StatusLabel.Caption := 'Installing Nimbo...';
    if not Exec('powershell.exe',
        '-NoProfile -ExecutionPolicy Bypass -File "' + ExpandConstant('{tmp}\setup-steps.ps1') + '"' +
        #ifndef NoDevCert
        ' -Cer "'  + ExpandConstant('{tmp}\NimboDev.cer') + '"' +
        #endif
        ' -Msix "' + ExpandConstant('{tmp}\Nimbo.msix') + '"',
        '', SW_HIDE, ewWaitUntilTerminated, rc) then
      rc := -1;
    if rc <> 0 then
      MsgBox('Nimbo could not be installed (step returned ' + IntToStr(rc) + ').' + #13#10 +
             'Try running Setup again, or install Nimbo.msix manually.', mbError, MB_OK);
  end;
end;
