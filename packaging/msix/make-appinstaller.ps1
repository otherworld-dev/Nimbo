# Generates Nimbo.appinstaller — the App Installer feed that lets installed
# copies auto-update. Host BOTH Nimbo.appinstaller and Nimbo.msix at a STABLE
# URL and install the FIRST time via the .appinstaller so Windows tracks updates:
#
#   Add-AppxPackage -AppInstallerFile "<BaseUrl>/Nimbo.appinstaller"
#
# GitHub hosting: use the release "latest/download" base, which always 302s to
# the newest release's assets (App Installer follows the redirect):
#
#   -BaseUrl https://github.com/<owner>/Nimbo/releases/latest/download
#
# For each new release: run package.ps1 (it bumps .build-rev), regenerate this
# (it reads the SAME revision so the feed version matches the MSIX), and publish
# both files. Windows re-checks the feed (HoursBetweenUpdateChecks) and updates.
#
# -Version is optional: when omitted it is derived from packaging/msix/.build-rev
# as 0.1.0.<rev> so the feed always matches the last package.ps1 build.
param(
    [string]$Version = "",
    [Parameter(Mandatory = $true)][string]$BaseUrl,  # e.g. https://github.com/adam/Nimbo/releases/latest/download
    [string]$Publisher = "CN=Nimbo Dev",             # MUST match the signing cert subject + manifest Publisher (see SIGNING.md)
    [string]$Name = "Nimbo"                          # MSIX Identity Name + feed/MSIX file basename; white-label passes the partner's
)
$ErrorActionPreference = "Stop"

# Derive the version from the last build's revision unless one was passed, so the
# feed can never advertise a version the hosted MSIX doesn't actually have.
if (-not $Version) {
    $revFile = Join-Path $PSScriptRoot ".build-rev"
    if (-not (Test-Path $revFile)) { throw "no .build-rev yet - run package.ps1 first, or pass -Version" }
    $rev = ((Get-Content $revFile -Raw).Trim())
    $Version = "0.1.0.$rev"
}

# Normalise to a 4-part version (Major.Minor.Build.Revision).
$parts = $Version.TrimStart('v').Split('.')
while ($parts.Count -lt 4) { $parts += "0" }
$ver = ($parts[0..3] -join '.')
$base = $BaseUrl.TrimEnd('/')

# The .appinstaller itself stays at the stable latest/download URL so update
# CHECKS always find the newest release. But the MSIX it pulls must use the
# IMMUTABLE per-version asset URL (.../releases/download/v<ver>/Nimbo.msix): the
# latest/download/Nimbo.msix alias is a CDN-cached redirect that can serve the
# PREVIOUS release's MSIX for a while after publishing — which made updates
# silently reinstall the old version ("installed ok", but no version change).
# A versioned URL is unique per release and never stale.
$msixUri = "$base/$Name.msix"
if ($base -match '^(?<root>.*)/releases/latest/download$') {
    $msixUri = "$($Matches.root)/releases/download/v$ver/$Name.msix"
}

$xml = @"
<?xml version="1.0" encoding="utf-8"?>
<AppInstaller xmlns="http://schemas.microsoft.com/appx/appinstaller/2021"
              Version="$ver"
              Uri="$base/$Name.appinstaller">
  <MainPackage Name="$Name"
               Publisher="$Publisher"
               Version="$ver"
               ProcessorArchitecture="x64"
               Uri="$msixUri" />
  <UpdateSettings>
    <OnLaunch HoursBetweenUpdateChecks="24" />
  </UpdateSettings>
</AppInstaller>
"@

$out = Join-Path $PSScriptRoot "$Name.appinstaller"
[System.IO.File]::WriteAllText($out, $xml, (New-Object System.Text.UTF8Encoding($false)))
Write-Host "Wrote $out (version $ver, base $base)"
Write-Host "Upload $Name.appinstaller + $Name.msix to $base/ and install via:"
Write-Host "  Add-AppxPackage -AppInstallerFile `"$base/$Name.appinstaller`""
