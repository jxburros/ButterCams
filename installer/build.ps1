# Builds the ButterCams installer on Windows.
#
#   pwsh installer\build.ps1 -Version 1.0.0
#
# Needs Go, Node.js and Inno Setup 6 (iscc.exe on PATH, or in its default folder).
param(
    [string]$Version = "1.0.0"
)
$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

Write-Host "== Building the web UI"
Push-Location web
npm ci
if ($LASTEXITCODE -ne 0) { throw "npm ci failed" }
npm run build
if ($LASTEXITCODE -ne 0) { throw "npm run build failed" }
Pop-Location

Write-Host "== Building buttercams.exe $Version"
New-Item -ItemType Directory -Force dist | Out-Null
$env:CGO_ENABLED = "0"
$env:GOOS = "windows"
$env:GOARCH = "amd64"
go build -trimpath -ldflags "-s -w -X github.com/jxburros/buttercams/internal/version.Version=$Version" -o dist\buttercams.exe .\cmd\buttercams
if ($LASTEXITCODE -ne 0) { throw "go build failed" }

Write-Host "== Building the installer"
$iscc = (Get-Command iscc.exe -ErrorAction SilentlyContinue).Source
if (-not $iscc) { $iscc = "${env:ProgramFiles(x86)}\Inno Setup 6\ISCC.exe" }
if (-not (Test-Path $iscc)) { throw "Inno Setup 6 (ISCC.exe) was not found" }
& $iscc "/DAppVersion=$Version" installer\buttercams.iss
if ($LASTEXITCODE -ne 0) { throw "ISCC failed" }
Write-Host "Installer written to dist\ButterCams-$Version-setup.exe"
