# Builds core/nimbo-core.aar from this repo's mobile/ facade via gomobile.
#
# Prereqs: Go; gomobile + gobind on PATH; Android SDK with an NDK under
# %ANDROID_HOME%\ndk\. The Go engine lives in this same repo, two levels up.
param(
    [string]$Out = (Join-Path $PSScriptRoot "..\core\nimbo-core.aar"),
    [string]$AndroidApi = "26",
    # ABIs to build. "android" = all four (ship builds); "android/arm64" alone
    # is ~4x faster and covers every modern device — use it for the dev loop.
    [string]$Targets = "android"
)
$ErrorActionPreference = "Stop"

if (-not $env:ANDROID_HOME) { $env:ANDROID_HOME = "$env:LOCALAPPDATA\Android\Sdk" }
if (-not $env:ANDROID_NDK_HOME) {
    $ndk = Get-ChildItem "$env:ANDROID_HOME\ndk" -Directory -ErrorAction SilentlyContinue |
        Sort-Object Name -Descending | Select-Object -First 1
    if ($ndk) { $env:ANDROID_NDK_HOME = $ndk.FullName }
}
if (-not (Test-Path "$env:ANDROID_NDK_HOME")) {
    throw "No NDK found under $env:ANDROID_HOME\ndk - install one (Android Studio SDK Manager or dl.google.com/android/repository)."
}
# The Go engine is in-tree: <repo>/android/scripts -> <repo>.
$RepoRoot = (Resolve-Path (Join-Path $PSScriptRoot "..\..")).Path
New-Item -ItemType Directory -Force (Split-Path $Out) | Out-Null
$Out = [System.IO.Path]::GetFullPath($Out)

Push-Location $RepoRoot
try {
    # gomobile requires the bound module to reference the bind runtime. Only
    # add it when it is missing: a blanket `go get` also upgrades every other
    # golang.org/x/* module in the repo's go.mod, silently moving the desktop
    # product's dependency set as a side effect of an Android build.
    $hasBind = (& go list -m golang.org/x/mobile 2>$null)
    if ($LASTEXITCODE -ne 0 -or -not $hasBind) {
        go get golang.org/x/mobile/bind
        if ($LASTEXITCODE -ne 0) { throw "go get golang.org/x/mobile/bind failed" }
    }
    # Android 15+ devices (and Google Play, since Nov 2025) require every packaged
    # .so to be 16 KB page-aligned. Go's default ELF layout is not, so the device
    # shows an "app compatibility" warning at launch and a future Play upload
    # would be rejected — pass the alignment through to the NDK linker.
    # .github/workflows/ci.yml (job android) runs the same gomobile bind; keep
    # the flags in both places in step.
    $ldflags = "-extldflags=-Wl,-z,max-page-size=16384"
    gomobile bind -target $Targets -androidapi $AndroidApi -javapkg dev.otherworld -ldflags $ldflags -o $Out ./mobile
    if ($LASTEXITCODE -ne 0) { throw "gomobile bind failed ($LASTEXITCODE)" }
} finally { Pop-Location }

Write-Host "Built $Out"
