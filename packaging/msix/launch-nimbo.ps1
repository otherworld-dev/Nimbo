# Launches the installed Nimbo package. Goes through explorer.exe so the app runs
# in the normal (non-elevated) user context even when started from the elevated
# installer.
$p = Get-AppxPackage | Where-Object { $_.Name -like "*Nimbo*" } | Select-Object -First 1
if ($p) {
    $id = (Get-AppxPackageManifest $p).Package.Applications.Application.Id
    Start-Process "explorer.exe" ("shell:AppsFolder\" + $p.PackageFamilyName + "!" + $id)
}
