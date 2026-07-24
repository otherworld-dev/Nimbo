# Builds, signs, and publishes a Nimbo release to GitHub so installed copies
# auto-update via the App Installer feed. One command per release:
#
#   .\release.ps1                       # build+sign, publish to the 'github' remote's repo
#   .\release.ps1 -Owner you -Repo Nimbo
#   .\release.ps1 -SkipBuild            # reuse the existing signed Nimbo.msix
#
# What it does:
#   1. package.ps1 -> signed Nimbo.msix (bumps .build-rev for an in-place update)
#   2. make-appinstaller.ps1 -> Nimbo.appinstaller pointing at the repo's stable
#      releases/latest/download URLs (version auto-derived from .build-rev)
#   3. gh release create/upload -> a release tagged v<version> carrying BOTH files
#
# Installed copies that were added via the .appinstaller feed re-check it on
# launch (HoursBetweenUpdateChecks) and update themselves.
#
# Prereqs: package.ps1's prereqs (Go, w64devkit gcc, Windows SDK, the CN=Nimbo
# Dev signing cert) and an authenticated gh CLI (`gh auth login`).
param(
    [string]$Owner = "",
    [string]$Repo = "Nimbo",
    [string]$Version = "0.1.0",
    [string]$SignSubject = "CN=Nimbo Dev",  # one knob for the signer/Publisher across MSIX, feed and installer (see SIGNING.md)

    # Azure Trusted Signing release: pass -AzureSign AND -SignSubject "<exact
    # issued subject>" (Trusted Signing account -> Certificate profiles ->
    # profile -> Subject). Prereq: az login as adam@otherworld.dev. See SIGNING.md.
    [switch]$AzureSign,
    [string]$AzureCertProfile = "otherworld-dev-ltd",
    [switch]$SkipBuild,
    [switch]$Force   # skip the clean-tree guard (deliberate WIP/test releases only)
)
$ErrorActionPreference = "Stop"
$here = $PSScriptRoot
$repoRoot = (Resolve-Path (Join-Path $here "..\..")).Path

# --- clean-tree guard: a release builds the WORKING TREE, not a commit, so a
# dirty checkout would silently ship uncommitted/half-finished work. Refuse
# unless -Force (deliberate test builds). Applies with -SkipBuild too: Setup.exe
# is still compiled from the tree during publish.
if (-not $Force) {
    $dirty = @(& git -C $repoRoot status --porcelain 2>$null) | Where-Object { $_ }
    if ($dirty) {
        Write-Host "Uncommitted changes in the working tree:" -ForegroundColor Yellow
        $dirty | ForEach-Object { Write-Host "  $_" }
        throw "release aborted: $($dirty.Count) uncommitted change(s) - commit first (or re-run with -Force to ship them anyway)"
    }
}

# --- locate gh (winget installs it here but it may not be on PATH yet) ---
$gh = (Get-Command gh -ErrorAction SilentlyContinue).Source
if (-not $gh) { $gh = "$env:ProgramFiles\GitHub CLI\gh.exe" }
if (-not (Test-Path $gh)) { throw "gh CLI not found - install it and run 'gh auth login'" }

# --- resolve Owner/Repo from the 'github' remote when not supplied ---
if (-not $Owner) {
    $url = (& git -C $repoRoot remote get-url github 2>$null)
    if ($url -and ($url -match 'github\.com[:/]+([^/]+)/([^/.]+)')) {
        $Owner = $Matches[1]
        if (-not $PSBoundParameters.ContainsKey('Repo')) { $Repo = $Matches[2] }
    }
}
if (-not $Owner) { throw "no -Owner given and no 'github' remote configured" }
Write-Host "Publishing to $Owner/$Repo"

# --- 1. build + sign ---
# Derive the next revision from the latest PUBLISHED release so versions are
# monotonic no matter which machine builds (the local .build-rev can reset or
# diverge across checkouts; a lower revision would silently stop auto-updates).
if (-not $SkipBuild) {
    $nextRev = 0
    $latestTag = (& $gh release view --repo "$Owner/$Repo" --json tagName --jq .tagName 2>$null)
    if ($LASTEXITCODE -eq 0 -and $latestTag -match '\.(\d+)\s*$') {
        $nextRev = [int]$Matches[1] + 1
        Write-Host "Latest release is $latestTag -> building revision $nextRev"
    } else {
        Write-Host "No prior release found; falling back to local .build-rev auto-bump"
    }
    & (Join-Path $here "package.ps1") -Version $Version -Revision $nextRev -SignSubject $SignSubject -AzureSign:$AzureSign -AzureCertProfile $AzureCertProfile
}
$msix = Join-Path $here "Nimbo.msix"
if (-not (Test-Path $msix)) { throw "Nimbo.msix not found - build first (omit -SkipBuild)" }

# --- 2. version from the revision package.ps1 just stamped ---
$rev = ((Get-Content (Join-Path $here ".build-rev") -Raw).Trim())
$pkgVersion = "$Version.$rev"
$tag = "v$pkgVersion"

# --- 3. App Installer feed pointing at the stable latest/download URLs ---
$base = "https://github.com/$Owner/$Repo/releases/latest/download"
& (Join-Path $here "make-appinstaller.ps1") -BaseUrl $base -Publisher $SignSubject   # -Version auto-derives from .build-rev
$appinstaller = Join-Path $here "Nimbo.appinstaller"

# Assets to publish: the MSIX + feed always; the offline Setup.exe too when it
# can be built. Setup.exe is uploaded under a STABLE name (Nimbo-Setup.exe) so
# the website can always link to .../releases/latest/download/Nimbo-Setup.exe.
$assets = @($msix, $appinstaller)
try {
    & (Join-Path $here "build-exe-installer.ps1") -Version $Version -SignSubject $SignSubject -AzureSign:$AzureSign -AzureCertProfile $AzureCertProfile
    $setupSrc = Join-Path $here "Nimbo-Setup-$Version.exe"
    if (Test-Path $setupSrc) {
        $setup = Join-Path $here "Nimbo-Setup.exe"
        Copy-Item $setupSrc $setup -Force
        $assets += $setup
    } else {
        Write-Warning "Setup.exe not produced; publishing without it."
    }
} catch {
    Write-Warning "Setup.exe build failed ($($_.Exception.Message)); publishing without it."
}

# --- changelog: commit subjects since the previous release was published ---
# These become the GitHub release notes, which the in-app update prompt shows
# ("what's in this update"). The window is the previous release's publishedAt
# (NOT createdAt, which is the tagged COMMIT's date and can be far older), so
# it works from any machine without local state.
$notes = "Automated release $tag"
$eap = $ErrorActionPreference; $ErrorActionPreference = 'Continue'
$prevCreated = (& $gh release view --repo "$Owner/$Repo" --json publishedAt --jq .publishedAt 2>$null)
$ErrorActionPreference = $eap
if ($prevCreated) {
    $subjects = @(& git -C $repoRoot log --since=$prevCreated --no-merges --pretty=%s 2>$null) |
        Where-Object { $_ } | ForEach-Object { ($_ -replace '\s*\[\+claude\]\s*$', '') }
    if ($subjects) { $notes = ($subjects | ForEach-Object { "- $_" }) -join "`n" }
}
# Pass notes via a file, not --notes "<string>": commit subjects routinely contain
# double-quotes/brackets that Windows PowerShell 5.1 mis-parses as native-command
# args (a quote in a subject aborted a publish with "no matches found").
$notesFile = Join-Path ([System.IO.Path]::GetTempPath()) "nimbo-release-notes.md"
[System.IO.File]::WriteAllText($notesFile, $notes, (New-Object System.Text.UTF8Encoding($false)))

# --- 4. publish the release with both assets ---
# Check existence without letting gh's stderr ("release not found") abort us:
# under $ErrorActionPreference=Stop, redirecting a native exe's stderr in
# Windows PowerShell turns it into a terminating NativeCommandError.
$eap = $ErrorActionPreference; $ErrorActionPreference = 'Continue'
& $gh release view $tag --repo "$Owner/$Repo" 2>&1 | Out-Null
$exists = ($LASTEXITCODE -eq 0)
$ErrorActionPreference = $eap
if ($exists) {
    Write-Host "Release $tag exists - replacing assets"
    & $gh release upload $tag @assets --repo "$Owner/$Repo" --clobber
} else {
    & $gh release create $tag @assets --repo "$Owner/$Repo" `
        --title "Nimbo $tag" --notes-file $notesFile
}
if ($LASTEXITCODE -ne 0) { throw "gh release failed" }

Write-Host ""
Write-Host "Published $tag to https://github.com/$Owner/$Repo/releases"
Write-Host "First-time install (so Windows tracks updates):"
Write-Host "  Add-AppxPackage -AppInstallerFile `"$base/Nimbo.appinstaller`""
