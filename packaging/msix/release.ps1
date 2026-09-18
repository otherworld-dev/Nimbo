# Builds and signs a Nimbo release, and publishes betas to GitHub so installed
# copies auto-update via the App Installer feed.
#
# Stable releases go through /release (merge dev into main, vX.Y.Z tag, changelog
# as release notes). This script is only its build step there; it publishes
# nothing but betas:
#
#   .\release.ps1 -NoPublish -Revision 0   # /release's build step: stable X.Y.Z.0, publish nothing
#   .\release.ps1 -PreRelease              # build + publish a beta X.Y.Z.N (beta channel only)
#   .\release.ps1 -PreRelease -SkipBuild   # re-publish the existing signed Nimbo.msix
#
# X.Y.Z comes from packaging/msix/VERSION unless -Version is given.
#
# What it does:
#   1. package.ps1 -> signed Nimbo.msix (bumps .build-rev for an in-place update)
#   2. make-appinstaller.ps1 -> Nimbo.appinstaller pointing at the repo's stable
#      releases/latest/download URLs (version auto-derived from .build-rev)
#   3. gh release create/upload -> a pre-release tagged vX.Y.Z.N carrying the files
#      (skipped with -NoPublish, where /release creates the vX.Y.Z release)
#
# Installed copies that were added via the .appinstaller feed re-check it on
# launch (HoursBetweenUpdateChecks) and update themselves.
#
# Prereqs: package.ps1's prereqs (Go, w64devkit gcc, Windows SDK, the CN=Nimbo
# Dev signing cert) and an authenticated gh CLI (`gh auth login`).
param(
    [string]$Owner = "",
    [string]$Repo = "Nimbo",
    [string]$Version = "",                 # X.Y.Z; empty = packaging/msix/VERSION
    [int]$Revision = -1,                   # passed to package.ps1: -1 = auto, 0 = stable (vX.Y.Z)
    [string]$SignSubject = "CN=Nimbo Dev",  # one knob for the signer/Publisher across MSIX, feed and installer (see the signing runbook)

    # Azure Trusted Signing release: pass -AzureSign AND -SignSubject "<exact
    # issued subject>" (Trusted Signing account -> Certificate profiles ->
    # profile -> Subject). Prereq: az login as adam@otherworld.dev. See the signing runbook.
    [switch]$AzureSign,
    [string]$AzureCertProfile = "otherworld-dev-ltd",
    [switch]$SkipBuild,

    # Publish as a GitHub PRE-RELEASE: the artefacts go up, but no installed copy
    # sees them. Both update paths skip pre-releases - the in-app updater filters
    # them (internal/update), and the .appinstaller feed points at
    # releases/latest/download, which GitHub excludes them from. Only installs
    # with the beta channel enabled in Settings pick it up. Betas are never
    # promoted: the stable comes from /release (promotion retired 2026-09-18).
    [switch]$PreRelease,
    [switch]$NoPublish,  # build, sign, feed and Setup.exe only - /release publishes
    [switch]$Force   # skip the clean-tree guard (deliberate WIP/test releases only)
)
$ErrorActionPreference = "Stop"
$here = $PSScriptRoot
$repoRoot = (Resolve-Path (Join-Path $here "..\..")).Path
. (Join-Path $here "rev-common.ps1")
if (-not $Version) { $Version = Get-BaseVersion }

# Stable releases are published by /release, so a stable publish from here would
# skip main, the vX.Y.Z tag and the changelog.
if (-not $NoPublish -and -not $PreRelease) {
    throw "stable releases go through /release - use -NoPublish for its build step, or -PreRelease for a beta"
}

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

# --- a published beta's tag must point at a commit GitHub already has ---
# The release is created with --target (not whatever GitHub's default branch
# points at), so it names exactly the commit that was built.
if (-not $NoPublish) {
    $eap = $ErrorActionPreference; $ErrorActionPreference = 'Continue'
    & git -C $repoRoot fetch --quiet github 2>$null
    $target = (& git -C $repoRoot rev-parse HEAD 2>$null)
    $onGithub = @(& git -C $repoRoot branch -r --contains $target --list 'github/*' 2>$null) | Where-Object { $_ }
    $ErrorActionPreference = $eap
    if (-not $onGithub) { throw "HEAD $target isn't on GitHub yet - push it first, so the release tag points at a published commit" }
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
# The revision comes from package.ps1's unified auto-bump (see rev-common.ps1):
# one above the higher of the local .build-rev and the newest GitHub release,
# so versions stay monotonic no matter which machine builds AND local test
# builds can never share a number with a release. If the GitHub query fails,
# package.ps1 falls back to the local counter alone - a stale counter would
# then collide with an existing tag and `gh release create` fails loudly
# rather than shipping a duplicate.
if (-not $SkipBuild) {
    & (Join-Path $here "package.ps1") -Version $Version -Revision $Revision -SignSubject $SignSubject -AzureSign:$AzureSign -AzureCertProfile $AzureCertProfile
}
$msix = Join-Path $here "Nimbo.msix"
if (-not (Test-Path $msix)) { throw "Nimbo.msix not found - build first (omit -SkipBuild)" }

# --- 2. version from the revision package.ps1 just stamped ---
$rev = ((Get-Content (Join-Path $here ".build-rev") -Raw).Trim())
$pkgVersion = "$Version.$rev"
$tag = Get-ReleaseTag -Version $Version -Revision ([int]$rev)   # vX.Y.Z for a stable, vX.Y.Z.N for a beta
if ($PreRelease -and [int]$rev -eq 0) {
    # Revision 0 is the stable's number, and its vX.Y.Z tag belongs to /release.
    throw "a beta can't be revision 0 (that is the $tag stable) - rebuild without -Revision 0"
}

# --- 3. App Installer feed pointing at the stable latest/download URLs ---
# -Tag matters: the feed links the MSIX by its release tag, and a stable's tag
# (vX.Y.Z) is not its 4-part package version.
$base = "https://github.com/$Owner/$Repo/releases/latest/download"
& (Join-Path $here "make-appinstaller.ps1") -BaseUrl $base -Publisher $SignSubject -Version $pkgVersion -Tag $tag
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

# --- /release's build step stops here: /release creates the vX.Y.Z release ---
if ($NoPublish) {
    Write-Host ""
    Write-Host "Built $pkgVersion for release tag $tag (not published). Assets:"
    $assets | ForEach-Object { Write-Host "  $_" }
    return
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
    if ($LASTEXITCODE -eq 0 -and $PreRelease) {
        # Only ever promote-to-pre-release here, never demote: a plain re-run
        # without -PreRelease must not silently make an existing pre-release
        # public.
        & $gh release edit $tag --repo "$Owner/$Repo" --prerelease
    }
} else {
    $extra = @()
    if ($PreRelease) { $extra += "--prerelease" }
    & $gh release create $tag @assets --repo "$Owner/$Repo" `
        --title "Nimbo $tag" --target $target --notes-file $notesFile @extra
}
if ($LASTEXITCODE -ne 0) { throw "gh release failed" }

# Report the release's ACTUAL visibility, not the requested -PreRelease value:
# the --clobber path above only flips it to pre-release when asked and never
# demotes, so "requested" and "actual" can differ. gh's --jq prints the bare
# JSON literal `true`/`false` as a STRING - PowerShell treats the string
# "false" as truthy, so compare it explicitly rather than using it as a
# condition on its own.
#
# This runs AFTER the release is already published, so a transient gh error
# here must not blow up the whole script (same reasoning as the exists-check
# above: redirecting a native command's stderr under $ErrorActionPreference =
# "Stop" still turns it into a terminating error even when redirected to
# $null) - save/restore ErrorActionPreference around the call, and if the
# query itself fails, fall back to the requested -PreRelease value rather than
# defaulting to "published, go ahead and tell the operator it's public".
$eap = $ErrorActionPreference; $ErrorActionPreference = 'Continue'
$isPrerelease = (& $gh release view $tag --repo "$Owner/$Repo" --json isPrerelease --jq .isPrerelease 2>$null)
$queryOk = ($LASTEXITCODE -eq 0)
$ErrorActionPreference = $eap
if (-not $queryOk) {
    Write-Warning "Couldn't confirm $tag's published visibility; assuming the requested -PreRelease value."
    $isPrerelease = if ($PreRelease) { "true" } else { "false" }
}

Write-Host ""
Write-Host "Published $tag to https://github.com/$Owner/$Repo/releases"
if ($isPrerelease -eq "true") {
    Write-Host "This is a PRE-RELEASE - only installs with the beta channel enabled will see it." -ForegroundColor Yellow
    Write-Host "The next stable comes from /release, not from promoting this beta."
} else {
    Write-Host "First-time install (so Windows tracks updates):"
    Write-Host "  Add-AppxPackage -AppInstallerFile `"$base/Nimbo.appinstaller`""
}
