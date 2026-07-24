# Installs (or updates) the signed Nimbo MSIX on this machine.
# Prereq: make-cert.ps1 has trusted the signing cert, and package.ps1 produced
# a SIGNED Nimbo.msix. Quit any running Nimbo first.
$ErrorActionPreference = "Stop"
$msix = Join-Path $PSScriptRoot "Nimbo.msix"
if (-not (Test-Path $msix)) { throw "Nimbo.msix not found - run package.ps1 first" }

Get-Process -Name "nimbo-gui" -ErrorAction SilentlyContinue | Stop-Process -Force
# In-place UPDATE (do NOT Remove first). package.ps1 bumps the revision each
# build, so Add-AppxPackage sees a higher version and updates in place. This
# preserves the package's data container (your login/config/sync DB) instead of
# wiping it. -ForceApplicationShutdown closes a running instance that's holding
# files; -ForceUpdateFromAnyVersion is a safety net if .build-rev was reset.
Add-AppxPackage -Path $msix -ForceApplicationShutdown -ForceUpdateFromAnyVersion
Write-Host "Installed (in-place update; login preserved). Launch Nimbo from the Start menu."
