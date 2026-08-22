# Install steps run by the Nimbo Setup.exe (Inno Setup), already elevated.
# Trusts the bundled signing certificate, then installs the app from the App
# Installer feed (so it gets the LATEST release and registers auto-updates),
# falling back to the bundled MSIX when offline.
param(
    [string]$Cer = "",   # dev-cert builds only; empty for Azure-signed builds (public chain, nothing to trust)
    [Parameter(Mandatory = $true)][string]$Msix,
    [string]$OverlayDll = "",    # Explorer badge DLL to place + register (optional)
    [string]$OverlayIcons = "",  # its icons folder
    [string]$Feed = "https://github.com/otherworld-dev/Nimbo/releases/latest/download/Nimbo.appinstaller"
)
$ErrorActionPreference = "Stop"

# A self-signed cert is its own root, so trust it in both stores Windows checks.
if ($Cer) {
    Import-Certificate -FilePath $Cer -CertStoreLocation Cert:\LocalMachine\TrustedPeople | Out-Null
    Import-Certificate -FilePath $Cer -CertStoreLocation Cert:\LocalMachine\Root | Out-Null
}

# Close a running copy, then install in place (preserves login/config/sync DB).
Get-Process -Name "nimbo-gui" -ErrorAction SilentlyContinue | Stop-Process -Force

# Prefer the feed: it installs the latest release AND registers App Installer
# update-tracking (a bare-MSIX install gets neither, and a stale bundled MSIX
# would DOWNGRADE a machine that already has a newer build). Fall back to the
# bundled MSIX only when the feed is unreachable; that copy won't auto-update
# until it's next installed online.
try {
    Add-AppxPackage -AppInstallerFile $Feed -ForceTargetApplicationShutdown -ErrorAction Stop
} catch {
    Add-AppxPackage -Path $Msix -ForceApplicationShutdown -ForceUpdateFromAnyVersion
}

# --- Explorer overlay badges (research-settled 2026-08-17, Deck #561) ---
#
# Icon-corner badges are the ONE piece of shell integration an MSIX cannot carry:
# Explorer enumerates HKLM\...\ShellIconOverlayIdentifiers only, refuses in-proc
# activation of packaged COM from identity-less processes, and cannot even map a
# WindowsApps DLL as an image. OneDrive - which declares the same manifest
# element we do - still ships an elevated installer for exactly this step.
#
# So Setup, the one elevated moment a direct-download install already has, puts
# an Authenticode-signed copy of the DLL in Program Files (admin-owned: a
# user-writable DLL loaded into explorer.exe would be a planting vector) and
# registers it classically. Best-effort by design: badges are cosmetic and must
# never fail the install. The Status column works without any of this.
if ($OverlayDll -and (Test-Path $OverlayDll)) {
    try {
        $shellDir = Join-Path $env:ProgramFiles "Nimbo\Shell"
        New-Item -ItemType Directory -Force -Path $shellDir | Out-Null
        $target = Join-Path $shellDir "NCOverlays.dll"

        $same = $false
        if (Test-Path $target) {
            $same = (Get-FileHash -Algorithm SHA256 $target).Hash -eq (Get-FileHash -Algorithm SHA256 $OverlayDll).Hash
        }
        if (-not $same) {
            try {
                Copy-Item -Force $OverlayDll $target -ErrorAction Stop
            } catch {
                # explorer.exe holds the previous copy open. Place the new one in
                # a side dir and re-point the registration; the old copy keeps
                # serving until the next Explorer restart, then goes unused.
                $side = Join-Path $shellDir ("v-" + [guid]::NewGuid().ToString("N").Substring(0, 8))
                New-Item -ItemType Directory -Force -Path $side | Out-Null
                $target = Join-Path $side "NCOverlays.dll"
                Copy-Item -Force $OverlayDll $target
            }
        }
        if ($OverlayIcons -and (Test-Path $OverlayIcons)) {
            $icoDir = Join-Path (Split-Path $target -Parent) "icons"
            New-Item -ItemType Directory -Force -Path $icoDir | Out-Null
            Copy-Item -Force (Join-Path $OverlayIcons "*.ico") $icoDir
        }

        # DllRegisterServer writes the CLSIDs (HKCR -> HKLM\Software\Classes at
        # admin) and the space-prefixed ShellIconOverlayIdentifiers names, using
        # the module's own path - i.e. the Program Files copy just installed.
        & "$env:SystemRoot\System32\regsvr32.exe" /s $target

        # Ask the running Explorer to pick up newly registered identifiers now.
        # Only works while free overlay slots remain (15 machine-wide); if it
        # does nothing, the next Explorer start loads them.
        Add-Type -Namespace NimboSetup -Name Shell -MemberDefinition `
            '[DllImport("shell32.dll")] public static extern int SHLoadNonloadedIconOverlayIdentifiers();'
        [NimboSetup.Shell]::SHLoadNonloadedIconOverlayIdentifiers() | Out-Null
    } catch {
        # Cosmetic feature: swallow everything rather than fail the install.
    }
}
