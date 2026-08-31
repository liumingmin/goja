# Install the goja-debug VS Code extension (Windows).
#
# Default: create a symbolic link from the VS Code extensions directory to
# this folder, so edits take effect immediately (requires an elevated
# PowerShell or Developer Mode). Use -Copy to copy the files instead.
#
# Usage:
#   powershell -ExecutionPolicy Bypass -File install.ps1          # symlink
#   powershell -ExecutionPolicy Bypass -File install.ps1 -Copy    # copy
#   powershell -ExecutionPolicy Bypass -File install.ps1 -Uninstall

param(
    [switch]$Copy,
    [switch]$Uninstall
)

$ErrorActionPreference = "Stop"

$extName   = "goja-debug"
$src       = Split-Path -Parent $MyInvocation.MyCommand.Path
$targetDir = Join-Path $env:USERPROFILE ".vscode\extensions"
$dest      = Join-Path $targetDir $extName

if ($Uninstall) {
    if (Test-Path $dest) {
        Remove-Item -Recurse -Force $dest
        Write-Host "Removed $dest"
    } else {
        Write-Host "Nothing to remove: $dest does not exist"
    }
    Write-Host "Restart VS Code to complete uninstallation."
    exit 0
}

if (-not (Test-Path $targetDir)) {
    New-Item -ItemType Directory -Path $targetDir -Force | Out-Null
}

if (Test-Path $dest) {
    Write-Host "Removing existing $dest ..."
    Remove-Item -Recurse -Force $dest
}

if ($Copy) {
    Copy-Item -Recurse $src $dest
    Write-Host "Copied extension to $dest"
} else {
    try {
        New-Item -ItemType SymbolicLink -Path $dest -Target $src | Out-Null
        Write-Host "Symlinked $dest -> $src"
    } catch {
        Write-Warning "Symlink failed ($($_.Exception.Message)). Falling back to copy."
        Write-Warning "Re-run as Administrator or enable Developer Mode to use symlinks."
        Copy-Item -Recurse $src $dest
        Write-Host "Copied extension to $dest"
    }
}

Write-Host "Done. Restart VS Code, then open a .js file and press F5."
