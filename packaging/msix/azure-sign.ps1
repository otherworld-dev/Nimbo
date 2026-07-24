# Signs files with Azure Trusted Signing (the company's public CA-trusted cert).
# Shared by package.ps1 (MSIX) and build-exe-installer.ps1 (Setup.exe).
#
#   .\azure-sign.ps1 -Path .\Nimbo.msix
#
# Auth: run `az login` once as adam@otherworld.dev (the account holds the
# "Artifact Signing Certificate Profile Signer" role). The signing itself is
# signtool with Microsoft's dlib, which picks up the Azure CLI credential.
#
# The dlib (Microsoft.Trusted.Signing.Client NuGet pkg) is auto-downloaded to
# %LOCALAPPDATA%\Nimbo\TrustedSigning on first use. Needs the .NET 8 runtime
# and an x64 signtool from a Windows SDK >= 10.0.22621.
#
# Trusted Signing certs are SHORT-LIVED (rotated every ~3 days), so every
# signature MUST carry an RFC 3161 timestamp or it dies with the cert — the
# timestamp server here is mandatory, not best-effort.
param(
    [Parameter(Mandatory = $true)][string[]]$Path,
    [string]$Endpoint    = "https://neu.codesigning.azure.net",  # account region: North Europe
    [string]$Account     = "otherworld-sign",
    [string]$CertProfile = "otherworld-dev-ltd"
)
$ErrorActionPreference = "Stop"

foreach ($f in $Path) { if (-not (Test-Path $f)) { throw "file to sign not found: $f" } }

# --- x64 signtool (the dlib is x64; an x86 signtool can't load it) ---
$signtool = Get-ChildItem "C:\Program Files (x86)\Windows Kits\10\bin" -Filter signtool.exe -Recurse -ErrorAction SilentlyContinue |
    Where-Object { $_.FullName -match '\\x64\\' } |
    Sort-Object FullName -Descending | Select-Object -First 1 -ExpandProperty FullName
if (-not $signtool) { throw "x64 signtool.exe not found (install the Windows SDK)" }

# --- the Trusted Signing dlib (auto-download + cache) ---
$cache = Join-Path $env:LOCALAPPDATA "Nimbo\TrustedSigning"
$dlib = Join-Path $cache "bin\x64\Azure.CodeSigning.Dlib.dll"
if (-not (Test-Path $dlib)) {
    Write-Host "Downloading Microsoft.Trusted.Signing.Client (signtool dlib)..."
    New-Item -ItemType Directory -Force $cache | Out-Null
    $zip = Join-Path $cache "client.nupkg.zip"
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    Invoke-WebRequest "https://www.nuget.org/api/v2/package/Microsoft.Trusted.Signing.Client" -OutFile $zip -UseBasicParsing
    Expand-Archive $zip -DestinationPath $cache -Force
    Remove-Item $zip
    if (-not (Test-Path $dlib)) { throw "dlib not found after download: $dlib" }
}
if (-not (dotnet --list-runtimes 2>$null | Select-String 'Microsoft\.NETCore\.App 8\.')) {
    Write-Warning "No .NET 8 runtime detected - the Trusted Signing dlib needs it (winget install Microsoft.DotNet.Runtime.8)."
}

# --- Azure CLI credential (the dlib authenticates as whoever `az login`-ed) ---
$az = (Get-Command az -ErrorAction SilentlyContinue).Source
if (-not $az) {
    $az = "$env:ProgramFiles\Microsoft SDKs\Azure\CLI2\wbin\az.cmd"
    if (Test-Path $az) { $env:Path = (Split-Path $az) + ";" + $env:Path } else { throw "Azure CLI not found - winget install Microsoft.AzureCLI, then az login" }
}
$eap = $ErrorActionPreference; $ErrorActionPreference = 'Continue'
& $az account show 2>&1 | Out-Null
$loggedIn = ($LASTEXITCODE -eq 0)
$ErrorActionPreference = $eap
if (-not $loggedIn) { throw "Azure CLI is not logged in - run: az login  (as adam@otherworld.dev)" }

# --- metadata.json tells the dlib which account/profile to sign with ---
$meta = Join-Path ([System.IO.Path]::GetTempPath()) "nimbo-azsign-metadata.json"
$json = @{
    Endpoint               = $Endpoint.TrimEnd('/')
    CodeSigningAccountName = $Account
    CertificateProfileName = $CertProfile
} | ConvertTo-Json
[System.IO.File]::WriteAllText($meta, $json, (New-Object System.Text.UTF8Encoding($false)))

Write-Host "Signing via Azure Trusted Signing ($Account/$CertProfile) ..."
& $signtool sign /v /fd SHA256 /tr "http://timestamp.acs.microsoft.com" /td SHA256 /dlib $dlib /dmdf $meta @Path
if ($LASTEXITCODE -ne 0) { throw "Azure Trusted Signing failed (signtool exit $LASTEXITCODE). Check az login account, RBAC role, and that certificate profile '$CertProfile' exists." }
Write-Host "Signed + timestamped: $($Path -join ', ')"
