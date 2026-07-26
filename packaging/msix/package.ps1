# Builds the Nimbo MSIX package: compiles the GUI exe (windowsgui subsystem) and
# the IExplorerCommand DLL, stages them with the manifest + logos, packs with
# makeappx, and signs if a code-signing cert is available.
#
# Usage:  .\package.ps1 [-Version 0.1.0]
#         .\package.ps1 -StoreChannel        # installable build that behaves as the Store one
# Prereqs: Go + w64devkit gcc on PATH; Windows SDK (makeappx/signtool); a cert
#          from make-cert.ps1 in Cert:\CurrentUser\My (CN=Nimbo Dev) to sign.
param(
    [string]$Version = "0.1.0",
    [int]$Revision = 0,                    # explicit 4th component; 0 = auto-bump from .build-rev
    [string]$SignSubject = "CN=Nimbo Dev", # signing cert subject; change when moving to a real CA cert (must match the manifest Publisher)

    # --- Azure Trusted Signing (-AzureSign) ---
    # Signs with the company's public CA-trusted cert via azure-sign.ps1 instead
    # of a local cert. -SignSubject must then be the EXACT subject issued by the
    # certificate profile (it still drives the manifest Publisher). See SIGNING.md.
    [switch]$AzureSign,
    [string]$AzureCertProfile = "otherworld-dev-ltd",

    # --- Microsoft Store build (-Store) ---
    # Produces an UNSIGNED Nimbo-Store.msix carrying the Store-assigned identity and
    # a self-update-disabled binary (channel=store, so it never tries to update from
    # the GitHub feed). Upload it to Partner Center, which signs it. Get the three
    # identity values from Partner Center -> your product -> Product management ->
    # Product identity, after reserving the app name.
    [switch]$Store,
    [string]$StoreIdentityName,            # Package/Identity Name, e.g. "1234Otherworld.Nimbo"
    [string]$StorePublisher,               # Store-assigned Publisher, e.g. "CN=ABCD1234-1234-..."
    [string]$StorePublisherDisplay = "Otherworld Dev Ltd",  # Publisher display name (must match Partner Center exactly)
    # Store listing title = the package's Properties>DisplayName, which must match a
    # reserved app name in Partner Center. The keyworded name ranks in Store search
    # ("nextcloud"/"sync"); VisualElements (Start menu) stays plain "Nimbo".
    [string]$StoreDisplayName = "Nimbo - Nextcloud Sync Client",

    # --- Testing the Store build's behaviour (-StoreChannel) ---
    # Builds an ordinary, dev-signed, normally-installable package whose binary
    # merely CLAIMS to be the Store build (channel=store) - the one flag a real
    # Store package differs by, minus the Store identity and minus being unsigned.
    #
    # It exists because a real -Store package can't be inspected: Partner Center
    # signs it, so it comes out unsigned and Windows won't install it. And a loose
    # dev build proves nothing either - the Store-only UI is gated on
    # CanApplyUpdate(), which is false on ANY unpackaged build, so the UI would
    # hide for the wrong reason.
    #
    #   .\package.ps1 -StoreChannel      -> install it, then check Settings > About:
    #                                       "Update now" and "Get beta releases
    #                                       early" must BOTH be absent.
    #
    # Never ship one of these: it's dev-signed under the normal identity but
    # self-update is disabled, so it can't update itself or be updated. Uninstall
    # it and reinstall the real build when you're done.
    [switch]$StoreChannel
)
$ErrorActionPreference = "Stop"

$here = $PSScriptRoot
$repo = (Resolve-Path (Join-Path $here "..\..")).Path
$stage = Join-Path $here "stage"
if ($Store -and (-not $StoreIdentityName -or -not $StorePublisher)) {
    throw "Store build needs -StoreIdentityName and -StorePublisher (Partner Center -> product -> Product identity)."
}
if ($AzureSign -and $SignSubject -eq "CN=Nimbo Dev") {
    # A Publisher that doesn't equal the Azure cert's subject is rejected at install.
    throw "-AzureSign needs -SignSubject set to the exact issued cert subject (portal: Trusted Signing account -> Certificate profiles -> $AzureCertProfile -> Subject). See SIGNING.md."
}
# -StoreChannel gets its own filename deliberately: it must never be mistakable
# for a release artifact, since `release.ps1 -SkipBuild` publishes whatever sits
# at Nimbo.msix and this build has self-update disabled.
$msixName = if ($Store) { "Nimbo-Store.msix" } elseif ($StoreChannel) { "Nimbo-StoreChannel.msix" } else { "Nimbo.msix" }
$msix = Join-Path $here $msixName

# Start from a clean stage so renamed/stale artifacts (e.g. an old
# nextclient-gui.exe, or a doubly-nested Assets\Assets) never get bundled.
if (Test-Path $stage) { Remove-Item -Recurse -Force $stage }
New-Item -ItemType Directory -Force -Path $stage | Out-Null

# Bump the MSIX revision (4th version component) on every build. A strictly
# higher version lets install.ps1 do an in-place UPDATE, which PRESERVES the
# package's data container (your login, config, sync DB). Without this, rebuilds
# keep version 0.1.0.0 with different contents, forcing a remove+add that wipes
# that container -> you'd be logged out each time. The user-facing version stays
# $Version; only this hidden revision moves. .build-rev is gitignored dev state.
$revFile = Join-Path $here ".build-rev"
if ($Store) {
    # The Store manages updates itself and RESERVES the 4th (revision) component,
    # which must be 0. Each submission only needs a strictly higher version, so
    # bump -Version (the 3rd/build part) per submission, e.g. 0.1.0 -> 0.1.1. No
    # .build-rev bump here (that counter is for the in-place direct-download path).
    $pkgVersion = "$Version.0"
    Write-Host "Store package version: $pkgVersion (revision pinned to 0 for the Store)"
} else {
    if ($Revision -gt 0) {
        # Caller supplied the revision (release.ps1 derives it from the latest GitHub
        # release so versions stay monotonic regardless of which machine builds).
        $rev = $Revision
    } else {
        $rev = 0
        if (Test-Path $revFile) { $rev = [int]((Get-Content $revFile -Raw).Trim()) }
        $rev++
    }
    if ($rev -gt 65535) { $rev = 1 }  # MSIX revision field maxes at 65535
    Set-Content -Path $revFile -Value $rev -Encoding ascii
    $pkgVersion = "$Version.$rev"
    Write-Host "Package version: $pkgVersion (revision auto-bumped for in-place update)"
}

# Make sure Go and the w64devkit C/C++ toolchain are reachable even if they're
# not on the user's PATH (search common install locations).
function Ensure-OnPath($exe, $candidates) {
    if (Get-Command $exe -ErrorAction SilentlyContinue) { return }
    foreach ($d in $candidates) {
        if ($d -and (Test-Path (Join-Path $d "$exe.exe"))) { $env:Path = "$d;" + $env:Path; return }
    }
    throw "$exe not found on PATH or in: $($candidates -join ', ')"
}
Ensure-OnPath "go"  @("$env:ProgramFiles\Go\bin")
# w64devkit's bin provides both g++ and gcc (needed for cgo).
Ensure-OnPath "g++" @("$env:LOCALAPPDATA\w64devkit\bin", "C:\w64devkit\bin", "$env:ProgramFiles\w64devkit\bin")

# --- locate Windows SDK tools (newest) ---
function Find-SdkTool($name) {
    Get-ChildItem "C:\Program Files (x86)\Windows Kits\10\bin" -Filter $name -Recurse -ErrorAction SilentlyContinue |
        Sort-Object FullName -Descending | Select-Object -First 1 -ExpandProperty FullName
}
$makeappx = Find-SdkTool "makeappx.exe"
$signtool = Find-SdkTool "signtool.exe"
$makepri  = Find-SdkTool "makepri.exe"
if (-not $makeappx) { throw "makeappx.exe not found (install the Windows SDK)" }

# --- build the GUI exe (no console) ---
Write-Host "Building GUI exe..."
$env:GOOS = "windows"; $env:CGO_ENABLED = "1"
Push-Location $repo
$ldflags = "-H windowsgui -X main.version=v$pkgVersion"
# Store build: disable in-app self-update. -StoreChannel sets the same flag on an
# otherwise-normal package so the Store-only UI gating can actually be tested.
if ($Store -or $StoreChannel) {
    $ldflags += " -X main.channel=store"
    if ($StoreChannel -and -not $Store) {
        Write-Host "-StoreChannel: building an installable package that behaves as the Store build (self-update disabled)" -ForegroundColor Yellow
    }
}
& go build -ldflags $ldflags -o (Join-Path $stage "nimbo-gui.exe") ./cmd/nimbo-gui
if ($LASTEXITCODE -ne 0) { Pop-Location; throw "go build failed" }
Pop-Location

# --- build the context-menu DLL ---
Write-Host "Building context-menu DLL..."
& (Join-Path $repo "shell\windows\ctxmenu\build.ps1")
Copy-Item -Force (Join-Path $repo "shell\windows\ctxmenu\out\NimboCtxMenu.dll") (Join-Path $stage "NimboCtxMenu.dll")

# --- stage manifest (bumped revision + Publisher matching the signer) + assets ---
# The source manifest stays at 0.1.0.0 / CN=Nimbo Dev; only the staged copy
# carries $pkgVersion and the active $SignSubject. Patching Publisher here keeps
# the package Identity in lock-step with the signing cert (a mismatch is rejected
# by Windows) so switching certs is just -SignSubject. NOTE: changing Publisher
# changes the PackageFamilyName = a new app identity (not an upgrade) -- see
# SIGNING.md before doing it on installed machines.
$manifest = Get-Content (Join-Path $here "AppxManifest.xml") -Raw
# Stamp ONLY the <Identity> Version. The (?<![A-Za-z]) lookbehind stops this also
# matching the Version= inside TargetDeviceFamily MinVersion="10.0.22000.0" (which
# would drop the Win11 floor and let the package look installable on Win10).
$manifest = $manifest -replace '(?<![A-Za-z])Version="\d+\.\d+\.\d+\.\d+"', "Version=`"$pkgVersion`""
$pub = if ($Store) { $StorePublisher } else { $SignSubject }
$manifest = $manifest -replace 'Publisher="[^"]*"', "Publisher=`"$pub`""
if ($Store) {
    # Store-assigned identity: the package Name and Publisher display name must
    # match exactly what Partner Center reserved or the submission is rejected.
    # (Scope the Name replace to the <Identity> element so TargetDeviceFamily
    # Name="Windows.Desktop" is left untouched.)
    $manifest = $manifest -replace '(<Identity Name=")[^"]*"', "`${1}$StoreIdentityName`""
    $manifest = $manifest -replace '<PublisherDisplayName>[^<]*</PublisherDisplayName>', "<PublisherDisplayName>$StorePublisherDisplay</PublisherDisplayName>"
    # Element form only (Properties>DisplayName) - the VisualElements DisplayName is an
    # attribute and deliberately untouched, so Start menu / taskbar keep showing "Nimbo".
    $manifest = $manifest -replace '<DisplayName>[^<]*</DisplayName>', "<DisplayName>$StoreDisplayName</DisplayName>"
}
[System.IO.File]::WriteAllText(
    (Join-Path $stage "AppxManifest.xml"), $manifest, (New-Object System.Text.UTF8Encoding($false)))
Copy-Item -Recurse -Force (Join-Path $here "Assets") (Join-Path $stage "Assets")

# --- resource index (resources.pri) so Windows resolves the qualified asset
# variants — specifically the "_altform-unplated" taskbar icons, without which
# Windows 11 draws a coloured plate behind the icon. makepri reads the staged
# manifest + Assets; the index is then bundled by makeappx. ---
if ($makepri) {
    Write-Host "Building resources.pri (unplated taskbar icons)..."
    $priconfig = Join-Path $stage "priconfig.xml"
    & $makepri createconfig /cf $priconfig /dq en-US /o | Out-Null
    Push-Location $stage   # makepri indexes relative to the project root
    & $makepri new /pr $stage /cf $priconfig /mn (Join-Path $stage "AppxManifest.xml") /of (Join-Path $stage "resources.pri") /o | Out-Null
    Pop-Location
    Remove-Item $priconfig -Force -ErrorAction SilentlyContinue
    if (-not (Test-Path (Join-Path $stage "resources.pri"))) { Write-Warning "resources.pri not produced; taskbar plate may persist." }
} else {
    Write-Warning "makepri.exe not found - skipping resources.pri (taskbar icon may show a plate)."
}

# --- pack (validates the manifest schema) ---
Write-Host "Packing MSIX..."
& $makeappx pack /d $stage /p $msix /o
if ($LASTEXITCODE -ne 0) { throw "makeappx failed" }
Write-Host "Packed: $msix"

# --- sign (optional; needs the signing cert) ---
# NOTE: the signing cert's Subject must equal the manifest Identity Publisher
# (AppxManifest.xml) or Windows rejects the package. Keep -SignSubject, the
# manifest Publisher, and make-appinstaller's -Publisher in sync. See SIGNING.md.
if ($Store) {
    # Store packages are signed by Microsoft at ingestion, so we deliberately do
    # NOT sign here. Upload $msix to Partner Center -> your submission -> Packages.
    Write-Host ""
    Write-Host "Store build complete (UNSIGNED on purpose - the Microsoft Store signs it):"
    Write-Host "  $msix"
    Write-Host "Next: run the Windows App Cert Kit on it, then upload to Partner Center."
} elseif ($AzureSign) {
    & (Join-Path $here "azure-sign.ps1") -Path $msix -CertProfile $AzureCertProfile
} else {
    $cert = Get-ChildItem Cert:\CurrentUser\My -CodeSigningCert -ErrorAction SilentlyContinue |
        Where-Object { $_.Subject -eq $SignSubject } | Select-Object -First 1
    if ($cert -and $signtool) {
        Write-Host "Signing with $($cert.Thumbprint) ($SignSubject)..."
        & $signtool sign /fd SHA256 /sha1 $cert.Thumbprint $msix
        if ($LASTEXITCODE -ne 0) { throw "signtool failed" }
        Write-Host "Signed."
    } else {
        Write-Warning "No '$SignSubject' code-signing cert found - package is UNSIGNED. Run make-cert.ps1 first, then re-run."
    }
}
