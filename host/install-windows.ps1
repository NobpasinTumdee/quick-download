<#
.SYNOPSIS
    Builds the Quick Download engine and registers it as a Chrome Native
    Messaging host for the current user.

.DESCRIPTION
    Three things have to line up before Chrome will talk to a native host:

      1. A host manifest JSON file describing the executable.
      2. A registry value pointing at that file:
         HKCU\Software\Google\Chrome\NativeMessagingHosts\com.downloader.app
         whose (Default) value is the FULL PATH of the manifest.
      3. The manifest's allowed_origins listing your extension id.

    This script does all three. Per-user (HKCU) needs no admin rights.

.PARAMETER ExtensionId
    The 32-character id Chrome assigns your unpacked extension. Load the
    extension first (chrome://extensions -> Developer mode -> Load unpacked)
    and copy the id it shows.

.PARAMETER SkipBuild
    Register only, do not run "go build" (useful if Go is not installed and
    you already have bin\quick-download.exe).

.EXAMPLE
    .\install-windows.ps1 -ExtensionId abcdefghijklmnopabcdefghijklmnop
#>

[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$ExtensionId,

    [switch]$SkipBuild
)

$ErrorActionPreference = 'Stop'

$HostName = 'com.downloader.app'
$RepoRoot = Split-Path -Parent $PSScriptRoot
$BinDir   = Join-Path $RepoRoot 'bin'
$ExePath  = Join-Path $BinDir 'quick-download.exe'
$Manifest = Join-Path $BinDir "$HostName.json"

function Write-Step($text) { Write-Host "==> $text" -ForegroundColor Cyan }
function Write-Ok($text)   { Write-Host "    $text" -ForegroundColor Green }

# --- 1. validate the extension id -------------------------------------------
# Chrome ids are exactly 32 characters drawn from a-p.
if ($ExtensionId -notmatch '^[a-p]{32}$') {
    throw "'$ExtensionId' is not a valid Chrome extension id (expected 32 characters, a-p only)."
}

# --- 2. build the engine ------------------------------------------------------
if (-not $SkipBuild) {
    Write-Step 'Building the Go engine'
    if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
        throw 'Go is not on PATH. Install Go 1.22+ or rerun with -SkipBuild.'
    }
    if (-not (Test-Path $BinDir)) { New-Item -ItemType Directory -Path $BinDir | Out-Null }

    Push-Location (Join-Path $RepoRoot 'backend')
    try {
        & go build -trimpath -ldflags '-s -w' -o $ExePath .
        if ($LASTEXITCODE -ne 0) { throw "go build failed with exit code $LASTEXITCODE" }
    }
    finally { Pop-Location }
    Write-Ok "built $ExePath"
}

if (-not (Test-Path $ExePath)) {
    throw "Engine binary not found at $ExePath. Run without -SkipBuild, or build it manually."
}

# --- 3. write the host manifest ----------------------------------------------
Write-Step 'Writing the native messaging host manifest'

# JSON needs backslashes escaped; ConvertTo-Json handles that for us.
$manifestObject = [ordered]@{
    name            = $HostName
    description     = 'Quick Download local engine'
    path            = (Resolve-Path $ExePath).Path
    type            = 'stdio'
    allowed_origins = @("chrome-extension://$ExtensionId/")
}

# Chrome rejects a manifest with a UTF-8 BOM, so write plain ASCII/UTF8 no-BOM.
$json = $manifestObject | ConvertTo-Json -Depth 5
[System.IO.File]::WriteAllText($Manifest, $json, (New-Object System.Text.UTF8Encoding($false)))
Write-Ok "wrote $Manifest"

# --- 4. register it with every Chromium browser present -----------------------
Write-Step 'Registering with Chromium-based browsers (HKCU, no admin needed)'

$browsers = @(
    @{ Name = 'Google Chrome';  Key = 'HKCU:\Software\Google\Chrome\NativeMessagingHosts' },
    @{ Name = 'Microsoft Edge'; Key = 'HKCU:\Software\Microsoft\Edge\NativeMessagingHosts' },
    @{ Name = 'Chromium';       Key = 'HKCU:\Software\Chromium\NativeMessagingHosts' },
    @{ Name = 'Brave';          Key = 'HKCU:\Software\BraveSoftware\Brave-Browser\NativeMessagingHosts' }
)

foreach ($browser in $browsers) {
    $keyPath = Join-Path $browser.Key $HostName
    New-Item -Path $keyPath -Force | Out-Null
    # The (Default) value must be the absolute path of the manifest file.
    Set-ItemProperty -Path $keyPath -Name '(Default)' -Value $Manifest
    Write-Ok "$($browser.Name): $keyPath"
}

Write-Host ''
Write-Step 'Done'
Write-Host @"
    Engine    : $ExePath
    Manifest  : $Manifest
    Extension : $ExtensionId

    Next steps:
      1. Fully restart Chrome (close every window - not just the tab).
      2. Open the extension popup: the dot should read "engine vX.Y.Z".
      3. Dashboard: http://127.0.0.1:9090

    Logs live in: $env:APPDATA\quick-download\
    Uninstall with: .\uninstall-windows.ps1
"@
