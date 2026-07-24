# Builds NimboCtxMenu.dll (Windows 11 IExplorerCommand context-menu handler)
# with MinGW (w64devkit). Statically linked so it has no MinGW deps inside
# explorer.exe. Requires g++ from w64devkit on PATH.  Run:  .\build.ps1
$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

# Locate w64devkit's g++ if it isn't already on PATH.
if (-not (Get-Command g++ -ErrorAction SilentlyContinue)) {
    foreach ($d in @("$env:LOCALAPPDATA\w64devkit\bin", "C:\w64devkit\bin", "$env:ProgramFiles\w64devkit\bin")) {
        if ($d -and (Test-Path (Join-Path $d "g++.exe"))) { $env:Path = "$d;" + $env:Path; break }
    }
}
if (-not (Get-Command g++ -ErrorAction SilentlyContinue)) { throw "g++ (w64devkit) not found on PATH" }

$out = Join-Path $PSScriptRoot "out"
New-Item -ItemType Directory -Force -Path $out | Out-Null

$args = @(
    "-shared", "-O2",
    "-static", "-static-libgcc", "-static-libstdc++",
    "ctxmenu.cpp", "ctxmenu.def",
    "-o", (Join-Path $out "NimboCtxMenu.dll"),
    "-lole32", "-luuid", "-lshell32"
)
& g++ @args
if ($LASTEXITCODE -ne 0) { throw "g++ failed ($LASTEXITCODE)" }
Write-Host "Built: $(Join-Path $out 'NimboCtxMenu.dll')"
