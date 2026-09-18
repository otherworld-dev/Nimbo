# Builds Nimbo-Setup-<ver>.exe (a single Inno Setup installer that bundles the
# signed MSIX + cert and installs them).
#
#   .\build-exe-installer.ps1            # bundle the current Nimbo.msix
#   .\build-exe-installer.ps1 -Build     # build+sign first, then bundle
#
# Prereqs: make-cert.ps1 has been run once (NimboDev.cer exists); package.ps1 has
# produced a signed Nimbo.msix (or pass -Build); Inno Setup is installed
# (winget install --id JRSoftware.InnoSetup).
param(
    [switch]$Build,
    [string]$Version = "",
    [string]$SignSubject = "CN=Nimbo Dev", # installer signing cert subject (see SIGNING.md)

    # Azure Trusted Signing: sign Setup.exe via azure-sign.ps1 and build it
    # WITHOUT the dev-cert trust step (the cert chains to a public root, so
    # nothing needs importing on user machines). See SIGNING.md.
    [switch]$AzureSign,
    [string]$AzureCertProfile = "otherworld-dev-ltd"
)
$ErrorActionPreference = "Stop"
$root = $PSScriptRoot
. (Join-Path $root "rev-common.ps1")
if (-not $Version) { $Version = Get-BaseVersion }   # X.Y.Z from packaging/msix/VERSION

function Find-SdkTool($name) {
    Get-ChildItem "C:\Program Files (x86)\Windows Kits\10\bin" -Filter $name -Recurse -ErrorAction SilentlyContinue |
        Sort-Object FullName -Descending | Select-Object -First 1 -ExpandProperty FullName
}

if ($Build) { & (Join-Path $root "package.ps1") -Version $Version -SignSubject $SignSubject -AzureSign:$AzureSign -AzureCertProfile $AzureCertProfile }

$required = @("Nimbo.msix", "Nimbo.iss")
if (-not $AzureSign) { $required += "NimboDev.cer" }  # Azure builds don't bundle/trust the dev cert
foreach ($f in $required) {
    if (-not (Test-Path (Join-Path $root $f))) { throw "$f not found in $root" }
}

# Setup's launch step and version info come from the MSIX being bundled, so they
# can't drift from it (dev-cert and white-label builds have other family names).
# PackageFamilyName = Name + "_" + Crockford base32 of the first 8 bytes of
# SHA-256(UTF-16LE Publisher), the same derivation Windows uses.
Add-Type -AssemblyName System.IO.Compression.FileSystem
$zip = [IO.Compression.ZipFile]::OpenRead((Join-Path $root "Nimbo.msix"))
try {
    $entry = $zip.Entries | Where-Object FullName -eq "AppxManifest.xml"
    $reader = New-Object IO.StreamReader($entry.Open())
    try { [xml]$manifest = $reader.ReadToEnd() } finally { $reader.Close() }
} finally { $zip.Dispose() }
$identity = $manifest.Package.Identity
$appId = @($manifest.Package.Applications.Application)[0].Id
$sha = [Security.Cryptography.SHA256]::Create().ComputeHash([Text.Encoding]::Unicode.GetBytes($identity.Publisher))
$bits = (($sha[0..7] | ForEach-Object { [Convert]::ToString($_, 2).PadLeft(8, '0') }) -join '') + '0'
$alphabet = '0123456789abcdefghjkmnpqrstvwxyz'
$pfn = $identity.Name + "_" + (-join (0..12 | ForEach-Object { $alphabet[[Convert]::ToInt32($bits.Substring($_ * 5, 5), 2)] }))
Write-Host "Bundling $($identity.Name) $($identity.Version) ($pfn!$appId)"

# Locate the Inno Setup compiler.
$iscc = Get-ChildItem "$env:LOCALAPPDATA\Programs", "C:\Program Files (x86)", "C:\Program Files" `
    -Filter "ISCC.exe" -Recurse -ErrorAction SilentlyContinue -Depth 4 |
    Select-Object -First 1 -ExpandProperty FullName
if (-not $iscc) { throw "ISCC.exe not found. Install Inno Setup: winget install --id JRSoftware.InnoSetup" }

$isccArgs = @("/DAppVer=$Version", "/DFileVer=$($identity.Version)", "/DPfn=$pfn", "/DPkgAppId=$appId")
if ($AzureSign) { $isccArgs += "/DNoDevCert=1" }
& $iscc @isccArgs (Join-Path $root "Nimbo.iss")
if ($LASTEXITCODE -ne 0) { throw "Inno Setup compile failed ($LASTEXITCODE)" }

$exe = Join-Path $root "Nimbo-Setup-$Version.exe"
if (-not (Test-Path $exe)) { throw "Expected output $exe not found" }

# Sign the installer with the same cert the MSIX uses (timestamped so the
# signature stays valid past cert expiry). Self-signed: shows a publisher rather
# than a blank one; it does not earn SmartScreen reputation (a real CA cert would).
if ($AzureSign) {
    & (Join-Path $root "azure-sign.ps1") -Path $exe -CertProfile $AzureCertProfile
    $mb = [math]::Round((Get-Item $exe).Length / 1MB, 1)
    Write-Host ""
    Write-Host "Built: $exe  ($mb MB)"
    Write-Host "Distribute it directly; users run it and approve the admin prompt."
    return
}
$signtool = Find-SdkTool "signtool.exe"
$cert = Get-ChildItem Cert:\CurrentUser\My -CodeSigningCert -ErrorAction SilentlyContinue |
    Where-Object { $_.Subject -eq $SignSubject } | Select-Object -First 1
if ($cert -and $signtool) {
    Write-Host "Signing installer with $($cert.Thumbprint)..."
    & $signtool sign /fd SHA256 /sha1 $cert.Thumbprint /tr http://timestamp.digicert.com /td SHA256 $exe
    if ($LASTEXITCODE -ne 0) {
        Write-Warning "Timestamping failed (offline?) - signing without a timestamp."
        & $signtool sign /fd SHA256 /sha1 $cert.Thumbprint $exe
    }
    if ($LASTEXITCODE -ne 0) { throw "signtool failed on the installer" }
    Write-Host "Signed."
} else {
    Write-Warning "No 'CN=Nimbo Dev' cert or signtool found - the installer is UNSIGNED."
}

$mb = [math]::Round((Get-Item $exe).Length / 1MB, 1)
Write-Host ""
Write-Host "Built: $exe  ($mb MB)"
Write-Host "Distribute it directly; users run it and approve the admin prompt."
