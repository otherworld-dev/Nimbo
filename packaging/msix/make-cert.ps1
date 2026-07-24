# Creates a self-signed code-signing certificate for signing the Nimbo MSIX, and
# trusts it on THIS machine so the package installs without a real CA cert.
#
# Run ONCE, ELEVATED (adds the cert to the machine trust stores). The cert
# subject MUST match the manifest's Identity Publisher (default CN=Nimbo Dev).
#
# -Years controls validity: 10 by default so the cert doesn't silently expire in
# a year (an expired signer breaks new builds, and a *replacement* self-signed
# cert isn't trusted by existing installs until they re-trust it -- see
# SIGNING.md). Regenerating (delete the old cert first) changes the thumbprint,
# so you must re-trust it and re-release; do it deliberately, not routinely.
param(
    [string]$Subject = "CN=Nimbo Dev",
    [int]$Years = 10
)
$ErrorActionPreference = "Stop"

# Trusting the cert requires writing to LocalMachine stores (must be admin).
$admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $admin) {
    throw "Run this script in an ELEVATED PowerShell (Administrator) - it must add the cert to the machine trust store."
}

$existing = Get-ChildItem Cert:\CurrentUser\My -CodeSigningCert -ErrorAction SilentlyContinue |
    Where-Object { $_.Subject -eq $Subject } | Select-Object -First 1
if ($existing) {
    Write-Host "Cert already exists: $($existing.Thumbprint) (expires $($existing.NotAfter.ToString('yyyy-MM-dd')))"
    $cert = $existing
} else {
    Write-Host "Creating self-signed code-signing cert ($Subject), valid $Years years..."
    $cert = New-SelfSignedCertificate -Type Custom -Subject $Subject `
        -KeyUsage DigitalSignature -FriendlyName "Nimbo Dev signing" `
        -CertStoreLocation "Cert:\CurrentUser\My" `
        -NotAfter (Get-Date).AddYears($Years) `
        -TextExtension @("2.5.29.37={text}1.3.6.1.5.5.7.3.3", "2.5.29.19={text}")
    Write-Host "Created: $($cert.Thumbprint) (expires $($cert.NotAfter.ToString('yyyy-MM-dd')))"
}

# Trust it for package validation. A self-signed cert is its own root, so install
# it into both TrustedPeople (where Add-AppxPackage looks) and the Trusted Root
# store (so the chain validates).
$cer = Join-Path $PSScriptRoot "NimboDev.cer"
Export-Certificate -Cert $cert -FilePath $cer | Out-Null
foreach ($store in @("Cert:\LocalMachine\TrustedPeople", "Cert:\LocalMachine\Root")) {
    Import-Certificate -FilePath $cer -CertStoreLocation $store | Out-Null
    Write-Host "Trusted in $store"
}
Write-Host "Done. Now run package.ps1 to build + sign, then install.ps1."
