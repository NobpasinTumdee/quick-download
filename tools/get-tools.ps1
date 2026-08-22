<#
.SYNOPSIS
    Downloads yt-dlp.exe and ffmpeg.exe into bin\ so streaming downloads work.

.DESCRIPTION
    This script fetches third-party binaries from the internet:

      yt-dlp  https://github.com/yt-dlp/yt-dlp/releases/latest  (Unlicense)
      ffmpeg  https://www.gyan.dev/ffmpeg/builds/               (GPL build)

    Nothing is downloaded until you confirm. If you would rather not run this,
    download both by hand and drop them in bin\ — see tools\README.md.

.PARAMETER Force
    Re-download even if the binaries are already present.
#>

[CmdletBinding()]
param([switch]$Force)

$ErrorActionPreference = 'Stop'
$RepoRoot = Split-Path -Parent $PSScriptRoot
$BinDir   = Join-Path $RepoRoot 'bin'

if (-not (Test-Path $BinDir)) { New-Item -ItemType Directory -Path $BinDir | Out-Null }

$ytdlp  = Join-Path $BinDir 'yt-dlp.exe'
$ffmpeg = Join-Path $BinDir 'ffmpeg.exe'

Write-Host 'This downloads two third-party executables from the internet:' -ForegroundColor Yellow
Write-Host '  yt-dlp.exe  <- github.com/yt-dlp/yt-dlp (Unlicense)'
Write-Host '  ffmpeg.exe  <- gyan.dev/ffmpeg/builds  (GPL build)'
Write-Host "  into: $BinDir"
$answer = Read-Host 'Continue? [y/N]'
if ($answer -notmatch '^[Yy]') { Write-Host 'Cancelled.'; return }

# --- yt-dlp: a single self-contained exe ---
if ($Force -or -not (Test-Path $ytdlp)) {
    Write-Host '==> Downloading yt-dlp' -ForegroundColor Cyan
    Invoke-WebRequest -Uri 'https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp.exe' `
                      -OutFile $ytdlp -UseBasicParsing
    Write-Host "    $ytdlp" -ForegroundColor Green
} else {
    Write-Host "    yt-dlp already present (use -Force to refresh)" -ForegroundColor DarkGray
}

# --- ffmpeg: ships as a zip, we only need one file out of it ---
if ($Force -or -not (Test-Path $ffmpeg)) {
    Write-Host '==> Downloading ffmpeg (this one is a ~80 MB zip)' -ForegroundColor Cyan
    $zip = Join-Path $env:TEMP 'ffmpeg-release-essentials.zip'
    $tmp = Join-Path $env:TEMP 'qd-ffmpeg'

    Invoke-WebRequest -Uri 'https://www.gyan.dev/ffmpeg/builds/ffmpeg-release-essentials.zip' `
                      -OutFile $zip -UseBasicParsing

    if (Test-Path $tmp) { Remove-Item $tmp -Recurse -Force }
    Expand-Archive -Path $zip -DestinationPath $tmp -Force

    $found = Get-ChildItem -Path $tmp -Filter 'ffmpeg.exe' -Recurse | Select-Object -First 1
    if ($null -eq $found) { throw 'ffmpeg.exe was not found inside the downloaded archive.' }

    Copy-Item $found.FullName $ffmpeg -Force
    Remove-Item $zip -Force
    Remove-Item $tmp -Recurse -Force
    Write-Host "    $ffmpeg" -ForegroundColor Green
} else {
    Write-Host "    ffmpeg already present (use -Force to refresh)" -ForegroundColor DarkGray
}

Write-Host ''
Write-Host 'Done. Verify with: curl http://127.0.0.1:9090/api/tools' -ForegroundColor Cyan
