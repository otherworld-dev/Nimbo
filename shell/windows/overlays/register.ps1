# Registers (or, with -Unregister, removes) the Nimbo Explorer overlay
# handlers. Overlay identifiers live under HKLM, so this self-elevates via UAC.
# Explorer is restarted afterward so the change takes effect immediately.
param([switch]$Unregister)

$dll = Join-Path $PSScriptRoot "out\NCOverlays.dll"
if (-not (Test-Path $dll)) { throw "Build first: .\build.ps1  (missing $dll)" }

# Re-launch elevated if not already admin.
$admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()
         ).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $admin) {
    $argList = "-NoProfile -ExecutionPolicy Bypass -File `"$PSCommandPath`""
    if ($Unregister) { $argList += " -Unregister" }
    Start-Process powershell -Verb RunAs -ArgumentList $argList
    return
}

if ($Unregister) {
    & regsvr32.exe /s /u $dll
    Write-Host "Unregistered Nimbo overlays."
} else {
    & regsvr32.exe /s $dll
    Write-Host "Registered Nimbo overlays from $dll"
}

# Restart Explorer so it reloads the overlay-handler list.
Write-Host "Restarting Explorer..."
Stop-Process -Name explorer -Force -ErrorAction SilentlyContinue
Start-Sleep -Seconds 1
if (-not (Get-Process explorer -ErrorAction SilentlyContinue)) { Start-Process explorer.exe }
Write-Host "Done."
