# Builds NCOverlays.dll (Explorer icon-overlay shell extension) with MinGW
# (w64devkit). Output goes to .\out\ alongside the icons it references.
#
# -OutDir builds somewhere else instead. package.ps1 uses that, because on a
# machine where the overlays are registered, explorer.exe has the DLL loaded and
# holds a write lock on it: the default output path then fails with "Permission
# denied" and would break packaging for no good reason.
#
# Requires g++ from w64devkit on PATH. Run:  .\build.ps1
param([string]$OutDir = "")
$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

$out = if ($OutDir) { $OutDir } else { Join-Path $PSScriptRoot "out" }
New-Item -ItemType Directory -Force -Path $out | Out-Null
New-Item -ItemType Directory -Force -Path (Join-Path $out "icons") | Out-Null

# Statically link the C++/GCC runtimes so the DLL has no MinGW dependencies when
# loaded into explorer.exe.
$dll = Join-Path $out "NCOverlays.dll"
# Icon resources (windres ships with w64devkit next to g++).
$res = Join-Path $out "overlays.res.o"
& windres overlays.rc -O coff -o $res
if ($LASTEXITCODE -ne 0) { throw "windres failed ($LASTEXITCODE)" }
$args = @(
    "-shared", "-O2",
    "-static", "-static-libgcc", "-static-libstdc++",
    "overlays.cpp", $res, "overlays.def",
    "-o", $dll,
    "-lole32", "-luuid", "-ladvapi32", "-lshlwapi"
)
& g++ @args
if ($LASTEXITCODE -ne 0) {
    if (Test-Path $dll) {
        throw "g++ failed ($LASTEXITCODE). If this says 'Permission denied', explorer.exe has the DLL loaded: build with -OutDir, or restart Explorer."
    }
    throw "g++ failed ($LASTEXITCODE)"
}

Copy-Item -Force (Join-Path $PSScriptRoot "icons\*.ico") (Join-Path $out "icons")
Write-Host "Built: $dll"
