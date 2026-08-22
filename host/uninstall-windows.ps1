<#
.SYNOPSIS
    Removes the Quick Download native messaging registration and stops the engine.
#>

[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$HostName = 'com.downloader.app'

Write-Host '==> Stopping the engine' -ForegroundColor Cyan
Get-Process -Name 'quick-download' -ErrorAction SilentlyContinue | ForEach-Object {
    $_ | Stop-Process -Force
    Write-Host "    stopped pid $($_.Id)" -ForegroundColor Green
}

Write-Host '==> Removing registry entries' -ForegroundColor Cyan
$keys = @(
    "HKCU:\Software\Google\Chrome\NativeMessagingHosts\$HostName",
    "HKCU:\Software\Microsoft\Edge\NativeMessagingHosts\$HostName",
    "HKCU:\Software\Chromium\NativeMessagingHosts\$HostName",
    "HKCU:\Software\BraveSoftware\Brave-Browser\NativeMessagingHosts\$HostName"
)
foreach ($key in $keys) {
    if (Test-Path $key) {
        Remove-Item -Path $key -Recurse -Force
        Write-Host "    removed $key" -ForegroundColor Green
    }
}

Write-Host ''
Write-Host 'Done. The binary in bin\ and your downloads were left untouched.'
Write-Host "Logs (safe to delete): $env:APPDATA\quick-download\"
