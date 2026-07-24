# Nimbo end-user installer.
#
# Run this (or double-click "Install Nimbo.bat") to install Nimbo. It trusts
# Nimbo's signing certificate so Windows will accept the package, then installs
# the app and launches it. Needs administrator rights (to trust the certificate
# and register the app), so it self-elevates.
#
# Files expected next to this script: Nimbo.msix, NimboDev.cer

$ErrorActionPreference = "Stop"
$here = Split-Path -Parent $MyInvocation.MyCommand.Path

# --- self-elevate if not already admin -------------------------------------
$admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $admin) {
    Write-Host "Nimbo installer needs administrator rights - prompting..."
    Start-Process powershell -Verb RunAs -ArgumentList "-NoProfile -ExecutionPolicy Bypass -File `"$($MyInvocation.MyCommand.Path)`""
    return
}

function Fail($msg) { Write-Host ""; Write-Host "ERROR: $msg" -ForegroundColor Red; Read-Host "Press Enter to close"; exit 1 }

$cer  = Join-Path $here "NimboDev.cer"
$msix = Join-Path $here "Nimbo.msix"
$feed = "https://github.com/otherworld-dev/Nimbo/releases/latest/download/Nimbo.appinstaller"
if (-not (Test-Path $cer))  { Fail "NimboDev.cer not found next to this installer." }
if (-not (Test-Path $msix)) { Fail "Nimbo.msix not found next to this installer." }

Write-Host ""
Write-Host "Installing Nimbo" -ForegroundColor Cyan
Write-Host "----------------"

# 1. Trust the signing certificate so Windows accepts the package. (A self-signed
#    cert is its own root, so it goes into both TrustedPeople and the Root store.)
Write-Host "1/3  Trusting Nimbo's signing certificate..."
try {
    foreach ($store in @("Cert:\LocalMachine\TrustedPeople", "Cert:\LocalMachine\Root")) {
        Import-Certificate -FilePath $cer -CertStoreLocation $store | Out-Null
    }
} catch { Fail "Couldn't trust the certificate: $($_.Exception.Message)" }

# 2. Install. Prefer the App Installer FEED: it pulls the LATEST release and
#    registers auto-update tracking (a bare-MSIX install gets neither, and the
#    bundled MSIX could be older than what's already installed -> a downgrade).
#    Fall back to the bundled MSIX only when the feed is unreachable (offline).
Write-Host "2/3  Installing the app (latest from the update feed)..."
Get-Process -Name "nimbo-gui" -ErrorAction SilentlyContinue | Stop-Process -Force
try {
    Add-AppxPackage -AppInstallerFile $feed -ForceTargetApplicationShutdown -ErrorAction Stop
} catch {
    Write-Host "    Update feed unreachable; installing the bundled copy..."
    try {
        Add-AppxPackage -Path $msix -ForceApplicationShutdown -ForceUpdateFromAnyVersion
    } catch { Fail "Install failed: $($_.Exception.Message)" }
}

# 3. Launch it.
Write-Host "3/3  Launching Nimbo..."
try {
    $pkg = Get-AppxPackage | Where-Object { $_.Name -like "*Nimbo*" } | Select-Object -First 1
    if ($pkg) {
        $appId = (Get-AppxPackageManifest $pkg).Package.Applications.Application.Id
        Start-Process "shell:AppsFolder\$($pkg.PackageFamilyName)!$appId"
    }
} catch { Write-Host "    (Couldn't auto-launch - open Nimbo from the Start menu.)" }

Write-Host ""
Write-Host "Done. Nimbo is installed and in your Start menu." -ForegroundColor Green
Read-Host "Press Enter to close"
