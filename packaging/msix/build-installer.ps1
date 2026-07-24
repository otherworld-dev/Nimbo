# Assembles a distributable, offline Nimbo installer: the signed MSIX, the signing
# certificate, and the self-elevating installer. Produces packaging/msix/dist/ and
# a zip a user can download, unzip, and run "Install Nimbo.bat".
#
#   .\build-installer.ps1            # bundle the current Nimbo.msix
#   .\build-installer.ps1 -Build     # build+sign first, then bundle
#
# Prereqs: make-cert.ps1 has been run once (so NimboDev.cer exists), and
# package.ps1 has produced a signed Nimbo.msix (or pass -Build to do it here).
param(
    [switch]$Build,
    [string]$Version = "0.1.0"
)
$ErrorActionPreference = "Stop"
$root = $PSScriptRoot

if ($Build) { & (Join-Path $root "package.ps1") -Version $Version }

$msix = Join-Path $root "Nimbo.msix"
$cer  = Join-Path $root "NimboDev.cer"
if (-not (Test-Path $msix)) { throw "Nimbo.msix not found - run package.ps1 first (or pass -Build)." }
if (-not (Test-Path $cer))  { throw "NimboDev.cer not found - run make-cert.ps1 once first." }

$dist = Join-Path $root "dist"
Remove-Item $dist -Recurse -Force -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Path $dist | Out-Null

Copy-Item $msix                              (Join-Path $dist "Nimbo.msix")
Copy-Item $cer                               (Join-Path $dist "NimboDev.cer")
Copy-Item (Join-Path $root "Install-Nimbo.ps1") $dist
Copy-Item (Join-Path $root "Install Nimbo.bat")  $dist

$readme = @"
Nimbo - install
===============

1. Right-click "Install Nimbo.bat" and choose "Run as administrator"
   (or just double-click it and approve the admin prompt).
2. That's it - Nimbo installs and opens, and is in your Start menu.

What the installer does: it trusts Nimbo's signing certificate so Windows
will accept the app, then installs it. Admin rights are needed only for those
two steps. To remove Nimbo later: Settings > Apps > Nimbo > Uninstall.
"@
Set-Content -Path (Join-Path $dist "README.txt") -Value $readme -Encoding ASCII

$zip = Join-Path $root "Nimbo-Setup-$Version.zip"
Remove-Item $zip -Force -ErrorAction SilentlyContinue
Compress-Archive -Path (Join-Path $dist "*") -DestinationPath $zip

Write-Host ""
Write-Host "Installer bundle: $dist"
Get-ChildItem $dist | Select-Object Name, @{N='SizeMB';E={[math]::Round($_.Length/1MB,1)}} | Format-Table -AutoSize
Write-Host "Zipped: $zip"
Write-Host "Distribute the zip; users unzip and run 'Install Nimbo.bat'."
