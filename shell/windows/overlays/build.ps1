# Builds NCOverlays.dll (Explorer icon-overlay shell extension) with MinGW
# (w64devkit). Output goes to .\out\ alongside the icons it references.
#
# Requires g++ from w64devkit on PATH. Run:  .\build.ps1
$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

$out = Join-Path $PSScriptRoot "out"
New-Item -ItemType Directory -Force -Path $out | Out-Null
New-Item -ItemType Directory -Force -Path (Join-Path $out "icons") | Out-Null

# Statically link the C++/GCC runtimes so the DLL has no MinGW dependencies when
# loaded into explorer.exe.
$args = @(
    "-shared", "-O2",
    "-static", "-static-libgcc", "-static-libstdc++",
    "overlays.cpp", "overlays.def",
    "-o", (Join-Path $out "NCOverlays.dll"),
    "-lole32", "-luuid", "-ladvapi32", "-lshlwapi"
)
& g++ @args
if ($LASTEXITCODE -ne 0) { throw "g++ failed ($LASTEXITCODE)" }

Copy-Item -Force (Join-Path $PSScriptRoot "icons\*.ico") (Join-Path $out "icons")
Write-Host "Built: $(Join-Path $out 'NCOverlays.dll')"
Get-ChildItem $out -Recurse | Select-Object FullName, Length | Format-Table -AutoSize
