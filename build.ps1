<#
.SYNOPSIS
    Builds the Go engine and compiles the extension's TypeScript.
#>
[CmdletBinding()]
param(
    [switch]$SkipExtension
)

$ErrorActionPreference = 'Stop'
$Root = $PSScriptRoot

Write-Host '==> Building the Go engine' -ForegroundColor Cyan
if (-not (Get-Command go -ErrorAction SilentlyContinue)) { throw 'Go 1.22+ is required on PATH.' }
$binDir = Join-Path $Root 'bin'
if (-not (Test-Path $binDir)) { New-Item -ItemType Directory -Path $binDir | Out-Null }

Push-Location (Join-Path $Root 'backend')
try {
    & go test ./...
    if ($LASTEXITCODE -ne 0) { throw "go test failed with exit code $LASTEXITCODE" }

    & go build -trimpath -ldflags '-s -w' -o (Join-Path $binDir 'quick-download.exe') .
    if ($LASTEXITCODE -ne 0) { throw "go build failed with exit code $LASTEXITCODE" }
}
finally { Pop-Location }
Write-Host "    bin\quick-download.exe" -ForegroundColor Green

if (-not $SkipExtension) {
    Write-Host '==> Compiling the extension' -ForegroundColor Cyan
    if (-not (Get-Command npm -ErrorAction SilentlyContinue)) { throw 'Node 18+ / npm is required on PATH.' }

    Push-Location (Join-Path $Root 'extension')
    try {
        if (-not (Test-Path 'node_modules')) { & npm install }
        & npm run build
        if ($LASTEXITCODE -ne 0) { throw "tsc failed with exit code $LASTEXITCODE" }
    }
    finally { Pop-Location }
    Write-Host '    extension\dist\' -ForegroundColor Green
}

Write-Host ''
Write-Host 'Next: load extension\ at chrome://extensions, copy its id, then run' -ForegroundColor Cyan
Write-Host '      host\install-windows.ps1 -ExtensionId <id>' -ForegroundColor Cyan
