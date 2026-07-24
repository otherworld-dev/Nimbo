# Install steps run by the Nimbo Setup.exe (Inno Setup), already elevated.
# Trusts the bundled signing certificate, then installs the app from the App
# Installer feed (so it gets the LATEST release and registers auto-updates),
# falling back to the bundled MSIX when offline.
param(
    [string]$Cer = "",   # dev-cert builds only; empty for Azure-signed builds (public chain, nothing to trust)
    [Parameter(Mandatory = $true)][string]$Msix,
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
