# Builds feathers.exe for Windows with the version stamped in, matching the
# pinned upstream Wings release. Run from anywhere; paths are resolved relative
# to this script's repository.
#
#   pwsh ./scripts/build-windows.ps1 [-Version 1.12.3] [-Output .\feathers.exe]

param(
    [string]$Version = "1.12.3",
    [string]$Output  = "feathers.exe"
)

$ErrorActionPreference = "Stop"

# Repository root = parent of the scripts/ directory.
$repo = Split-Path -Parent $PSScriptRoot

$goCmd = Get-Command go -ErrorAction SilentlyContinue
if ($goCmd) { $go = $goCmd.Source } else { $go = "C:\Program Files\Go\bin\go.exe" }
if (-not (Test-Path $go)) { throw "Go toolchain not found. Install Go or set the path." }

$env:GOOS = "windows"
$env:GOARCH = "amd64"
$env:CGO_ENABLED = "0"

$ldflags = "-s -w -X github.com/pterodactyl/wings/system.Version=$Version"

Write-Host "Building feathers.exe (version $Version) ..."
& $go -C $repo build -ldflags $ldflags -o $Output .
if ($LASTEXITCODE -ne 0) { throw "build failed" }

Write-Host "Built: $Output"
& $go version
