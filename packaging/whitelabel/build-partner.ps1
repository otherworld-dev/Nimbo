# Builds a white-label partner's signed MSIX + App Installer feed from a profile
# under packaging/whitelabel/partners/<slug>/. One command per partner:
#
#   .\build-partner.ps1 -Partner example
#
# A profile dir holds two committed files:
#   brand.json    - the embedded brand for this build (becomes internal/brand/
#                   brand.json): name, accent, feedUrl, apiBase, appId, ...
#   partner.json  - the packaging identity + signing + hosting:
#                   { identityName, publisher, displayName, publisherDisplayName, feedBaseUrl }
#
# It swaps in the partner's brand.json, regenerates the icons from their accent,
# patches the MSIX manifest identity, builds + signs via the EXISTING package.ps1
# (with their cert), emits the .appinstaller via make-appinstaller, copies the
# artifacts to the profile's dist/, and ALWAYS restores the stock source files
# (git checkout) so a stock release can never accidentally ship partner branding.
#
# Prereqs: package.ps1's prereqs (Go, w64devkit gcc + windres, Windows SDK) and
# the partner's signing cert (subject = partner.json publisher) in
# Cert:\CurrentUser\My. A CLEAN git working tree for the files it mutates.
param(
    [Parameter(Mandatory = $true)][string]$Partner
)
$ErrorActionPreference = "Stop"
$here    = $PSScriptRoot                                       # packaging/whitelabel
$msix    = (Resolve-Path (Join-Path $here "..\msix")).Path
$repo    = (Resolve-Path (Join-Path $here "..\..")).Path
$profileDir = Join-Path $here "partners\$Partner"
if (-not (Test-Path $profileDir)) { throw "no partner profile at $profileDir" }

$brandSrc   = Join-Path $profileDir "brand.json"
$partnerCfg = Join-Path $profileDir "partner.json"
foreach ($f in @($brandSrc, $partnerCfg)) { if (-not (Test-Path $f)) { throw "missing $f" } }
$cfg = Get-Content $partnerCfg -Raw | ConvertFrom-Json
foreach ($k in 'identityName','publisher','displayName','publisherDisplayName','feedBaseUrl') {
    if (-not $cfg.$k) { throw "partner.json is missing '$k'" }
}
$appId = (Get-Content $brandSrc -Raw | ConvertFrom-Json).appId
if (-not $appId) { throw "brand.json is missing 'appId'" }

# Make Go + windres reachable even if not on PATH.
function Ensure-Tool($exe, $candidates) {
    if (Get-Command $exe -ErrorAction SilentlyContinue) { return }
    foreach ($d in $candidates) { if ($d -and (Test-Path (Join-Path $d "$exe.exe"))) { $env:Path = "$d;" + $env:Path; return } }
    throw "$exe not found on PATH or in: $($candidates -join ', ')"
}
Ensure-Tool "go"      @("$env:ProgramFiles\Go\bin")
Ensure-Tool "windres" @("$env:LOCALAPPDATA\w64devkit\bin", "C:\w64devkit\bin", "$env:ProgramFiles\w64devkit\bin")

# The build mutates these committed stock files; we restore them at the end. Bail
# if any already has uncommitted changes so we never clobber stock work-in-progress.
$stock = @(
    "internal/brand/brand.json",
    "cmd/nimbo-gui/assets/nimbo.ico",
    "cmd/nimbo-gui/rsrc_windows_amd64.syso",
    "packaging/msix/AppxManifest.xml",
    "packaging/msix/Assets"
)
$dirty = @(& git -C $repo status --porcelain -- $stock) | Where-Object { $_ }
if ($dirty) { throw "stock files have uncommitted changes; commit or stash them first:`n$($dirty -join "`n")" }

function Restore-Stock {
    & git -C $repo checkout -- $stock 2>&1 | Out-Null
}

try {
    Write-Host "== white-label build: $($cfg.identityName)  (publisher $($cfg.publisher)) =="

    # 1) Swap in the partner's embedded brand.
    Copy-Item -Force $brandSrc (Join-Path $repo "internal\brand\brand.json")

    # 2) Regenerate icons from the partner accent (auto-derived from brand.json).
    Push-Location (Join-Path $repo "shell\windows\appicon")
    try {
        & go run .                              # -> cmd/nimbo-gui/assets/nimbo.ico
        if ($LASTEXITCODE) { throw "appicon (ico) failed" }
        & go run . logos (Join-Path $msix "Assets")   # -> MSIX PNG logos
        if ($LASTEXITCODE) { throw "appicon (logos) failed" }
    } finally { Pop-Location }

    # 2b) Rebuild the resource object so the EXE/window icon embeds the new ico.
    Push-Location (Join-Path $repo "cmd\nimbo-gui")
    try {
        & windres app.rc -O coff -o rsrc_windows_amd64.syso
        if ($LASTEXITCODE) { throw "windres failed" }
    } finally { Pop-Location }

    # 3) Patch the SOURCE manifest identity. package.ps1 reads it and layers Version
    #    + Publisher into the STAGED copy; git checkout restores it afterwards.
    $mf = Join-Path $msix "AppxManifest.xml"
    $m = Get-Content $mf -Raw
    $m = $m -replace '(<Identity Name=")[^"]*"',               "`${1}$($cfg.identityName)`""
    $m = $m -replace '<DisplayName>[^<]*</DisplayName>',       "<DisplayName>$($cfg.displayName)</DisplayName>"
    $m = $m -replace '<PublisherDisplayName>[^<]*</PublisherDisplayName>', "<PublisherDisplayName>$($cfg.publisherDisplayName)</PublisherDisplayName>"
    $m = $m -replace '(<Application Id=")[^"]*"',              "`${1}$appId`""
    $m = $m -replace '(<uap:VisualElements[^>]*?DisplayName=")[^"]*"', "`${1}$($cfg.displayName)`""
    $m = $m -replace '(StartupTask[^>]*?DisplayName=")[^"]*"', "`${1}$($cfg.displayName)`""
    # The launcher alias must be brand-unique too (Go derives it from brand appId,
    # lowercased + "-app" — keep in step with launcherAlias() in shortcut_windows.go).
    $m = $m -replace '(<desktop:ExecutionAlias Alias=")[^"]*"', "`${1}$($appId.ToLower())-app.exe`""
    [System.IO.File]::WriteAllText($mf, $m, (New-Object System.Text.UTF8Encoding($false)))

    # 4) Build + sign with the partner's cert (package.ps1 owns Version + Publisher
    #    + build + sign). Unsigned (with a warning) if their cert isn't installed.
    & (Join-Path $msix "package.ps1") -SignSubject $cfg.publisher
    $builtMsix = Join-Path $msix "Nimbo.msix"
    if (-not (Test-Path $builtMsix)) { throw "package.ps1 did not produce Nimbo.msix" }

    # 5) App Installer feed, named for the partner's package + their hosting.
    & (Join-Path $msix "make-appinstaller.ps1") -Name $cfg.identityName -BaseUrl $cfg.feedBaseUrl -Publisher $cfg.publisher
    $builtFeed = Join-Path $msix "$($cfg.identityName).appinstaller"

    # 6) Collect artifacts under the profile's dist/ (gitignored).
    $dist = Join-Path $profileDir "dist"
    New-Item -ItemType Directory -Force -Path $dist | Out-Null
    Copy-Item -Force $builtMsix (Join-Path $dist "$($cfg.identityName).msix")
    Copy-Item -Force $builtFeed (Join-Path $dist "$($cfg.identityName).appinstaller")

    Write-Host ""
    Write-Host "Built partner package -> $dist"
    Get-ChildItem $dist | Select-Object Name, @{n='MB';e={[math]::Round($_.Length/1MB,2)}} | Format-Table -AutoSize | Out-String | Write-Host
    Write-Host "Host both files at $($cfg.feedBaseUrl)/ and first-install via:"
    Write-Host "  Add-AppxPackage -AppInstallerFile `"$($cfg.feedBaseUrl)/$($cfg.identityName).appinstaller`""
}
finally {
    Write-Host "Restoring stock source files (git checkout)..."
    Restore-Stock
    # Don't leave the partner build masquerading as the stock Nimbo.msix in the
    # release staging area, or a later `release.ps1 -SkipBuild` would publish it
    # as a Nimbo release. The partner copies already live under the profile's dist/.
    Remove-Item (Join-Path $msix "Nimbo.msix") -ErrorAction SilentlyContinue
    Remove-Item (Join-Path $msix "$($cfg.identityName).appinstaller") -ErrorAction SilentlyContinue
}
