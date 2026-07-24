# Rebuilds and re-registers NCOverlays.dll after the NextClient->Nimbo rename.
# The DLL is a registered shell overlay handler, so Explorer keeps it loaded and
# locked; this unregisters the old handlers (removing the NextClient* keys),
# stops Explorer to release the lock, rebuilds in place, registers the new Nimbo*
# handlers, and restarts Explorer.
#
# Run ONCE in an ELEVATED PowerShell (overlay handlers live under HKLM).
$ErrorActionPreference = "Stop"
$here = $PSScriptRoot
$dll  = Join-Path $here "out\NCOverlays.dll"

$admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $admin) { throw "Run this in an ELEVATED PowerShell (Administrator)." }

# Locate w64devkit g++ if not on PATH.
if (-not (Get-Command g++ -ErrorAction SilentlyContinue)) {
    foreach ($d in @("$env:LOCALAPPDATA\w64devkit\bin", "C:\w64devkit\bin", "$env:ProgramFiles\w64devkit\bin")) {
        if (Test-Path (Join-Path $d "g++.exe")) { $env:Path = "$d;" + $env:Path; break }
    }
}
if (-not (Get-Command g++ -ErrorAction SilentlyContinue)) { throw "g++ (w64devkit) not found." }

# 1) Unregister whatever the current out\NCOverlays.dll last registered.
if (Test-Path $dll) { & regsvr32 /u /s $dll }

# 1b) Purge any leftover NextClient* overlay keys directly. The current DLL's
# DllUnregisterServer only removes the Nimbo* keys, so if out\ was already rebuilt
# to Nimbo (or cleared), the old NextClient* keys would survive as stale handlers
# pointing at a missing DLL and waste overlay slots. Remove them by name; every
# other vendor's overlays are left untouched.
$ovRoot = 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Explorer\ShellIconOverlayIdentifiers'
Get-ChildItem $ovRoot -ErrorAction SilentlyContinue |
    Where-Object { $_.PSChildName.Trim() -like 'NextClient*' } |
    ForEach-Object {
        Write-Host "Removing stale overlay key: '$($_.PSChildName)'"
        Remove-Item $_.PSPath -Recurse -Force -ErrorAction SilentlyContinue
    }

# 2) Stop Explorer to release the DLL lock.
Stop-Process -Name explorer -Force -ErrorAction SilentlyContinue
Start-Sleep -Seconds 2

# 3) Rebuild in place (now unlocked).
& (Join-Path $here "build.ps1")

# 4) Register the new dll (writes the Nimbo* overlay keys).
& regsvr32 /s $dll

# 5) Restart Explorer so it picks up the new handlers.
if (-not (Get-Process -Name explorer -ErrorAction SilentlyContinue)) { Start-Process explorer.exe }
Remove-Item (Join-Path $here "out\NCOverlays-new.dll") -ErrorAction SilentlyContinue
Write-Host "Rebuilt + re-registered NCOverlays.dll with Nimbo overlay handlers."
