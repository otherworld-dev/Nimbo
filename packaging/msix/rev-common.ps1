# Shared version + revision derivation, dot-sourced by package.ps1, release.ps1,
# make-appinstaller.ps1, build-exe-installer.ps1 and build-installer.ps1.
#
# Every build is X.Y.Z.<revision>. X.Y.Z comes from packaging/msix/VERSION, which
# /release bumps. A stable release is revision 0 and is tagged vX.Y.Z by /release;
# betas and local builds take the next revision and are tagged vX.Y.Z.N.
#
# Local test builds and GitHub releases share ONE monotonic revision sequence
# (Adam's call, 2026-07-26): every build - local or released - goes one above
# the higher of the local .build-rev counter and the newest GitHub release, so
# a given version number names exactly one build. Before this, the two paths
# counted independently and the same number ("0.1.0.172") ended up naming a
# pre-fix local test build AND the fixed public release on the same day.

# Get-HighestReleaseRevision returns the highest 4th version component among
# recent GitHub releases, or -1 when it cannot be determined (no gh, offline,
# unauthenticated, no releases). Callers must treat -1 as "unknown, use the
# local counter alone" - never as zero.
#
# Uses `release list` (not `release view`, which resolves GitHub's "latest" and
# excludes pre-releases) so pre-releases count too - but --exclude-drafts stays
# on, since a hand-created draft would otherwise feed a bogus/missing tag into
# the derivation.
#
# `release list` sorts by createdAt, which is the TAGGED COMMIT's date, not the
# release's publish time. Several releases can share one commit (before the
# 2026-09-18 move to mirrored history every tag landed on the latest source
# snapshot), so ties happen - position 0 under a tie is not guaranteed to be the
# highest revision. Take the max revision across a window of recent releases
# instead of trusting sort order, so a tie can never derive a revision that
# collides with an existing release.
function Get-HighestReleaseRevision {
    param(
        [Parameter(Mandatory)][string]$Owner,
        [Parameter(Mandatory)][string]$Repo
    )
    $gh = (Get-Command gh -ErrorAction SilentlyContinue).Source
    if (-not $gh) { $gh = "$env:ProgramFiles\GitHub CLI\gh.exe" }
    if (-not (Test-Path $gh)) { return -1 }
    # Under $ErrorActionPreference=Stop, redirecting a native exe's stderr in
    # Windows PowerShell turns it into a terminating NativeCommandError - which
    # would abort the caller outright instead of reaching the fallback.
    $eap = $ErrorActionPreference; $ErrorActionPreference = 'Continue'
    $tags = @(& $gh release list --repo "$Owner/$Repo" --limit 20 --exclude-drafts --json tagName --jq '.[].tagName' 2>$null)
    $ok = ($LASTEXITCODE -eq 0)
    $ErrorActionPreference = $eap
    if (-not $ok) { return -1 }
    # Only 4-part tags carry a revision. A stable's vX.Y.Z tag would otherwise
    # read as revision Z.
    $maxRev = ($tags | ForEach-Object { if ($_ -match '^v?\d+\.\d+\.\d+\.(\d+)\s*$') { [int]$Matches[1] } } |
               Measure-Object -Maximum).Maximum
    if ($null -eq $maxRev) { return -1 }
    return [int]$maxRev
}

# Get-BaseVersion returns X.Y.Z from packaging/msix/VERSION.
function Get-BaseVersion {
    $file = Join-Path $PSScriptRoot "VERSION"
    if (-not (Test-Path $file)) { throw "packaging/msix/VERSION not found" }
    $v = (Get-Content $file -Raw).Trim()
    if ($v -notmatch '^\d+\.\d+\.\d+$') { throw "packaging/msix/VERSION must be X.Y.Z, got '$v'" }
    return $v
}

# Get-ReleaseTag names the GitHub release for a build: vX.Y.Z for a stable
# (revision 0), vX.Y.Z.N for anything else. The App Installer feed links the
# MSIX by this tag, so it has to match the release exactly.
function Get-ReleaseTag {
    param(
        [Parameter(Mandatory)][string]$Version,
        [Parameter(Mandatory)][int]$Revision
    )
    if ($Revision -eq 0) { return "v$Version" }
    return "v$Version.$Revision"
}

# Resolve-GitHubOwnerRepo derives owner/repo from the 'github' git remote.
# Returns a hashtable @{Owner=..; Repo=..} or $null when there is no remote.
function Resolve-GitHubOwnerRepo {
    param([Parameter(Mandatory)][string]$RepoRoot)
    $url = (& git -C $RepoRoot remote get-url github 2>$null)
    if ($url -and ($url -match 'github\.com[:/]+([^/]+)/([^/.]+)')) {
        return @{ Owner = $Matches[1]; Repo = $Matches[2] }
    }
    return $null
}
