# Starhold Companion — one-shot installer for Windows.
#
#   irm https://raw.githubusercontent.com/Wrecht-Dev/starhold-companion/main/install.ps1 | iex
#
# What it does:
#   1. downloads the latest release binary from GitHub
#   2. installs it to %LOCALAPPDATA%\StarholdCompanion
#   3. registers the native-messaging manifest for Chrome / Edge / Brave /
#      Firefox via HKCU registry keys (no admin rights needed)
#   4. prints the two steps left (extension + pairing)
#
# Safe to re-run (upgrades in place). No admin required — everything installs
# per-user.

$ErrorActionPreference = 'Stop'

$Repo = 'Wrecht-Dev/starhold-companion'
$BinName = 'wyrmhoard-companion.exe'
$ManifestId = 'cloud.mithraic.wyrmhoard_companion'
$ExtId = 'okbmiemolnnocdcmohnggigaolobdbef'
$InstallDir = Join-Path $env:LOCALAPPDATA 'StarholdCompanion'

function Log($m)  { Write-Host "==> $m" -ForegroundColor Cyan }
function Warn($m) { Write-Host "!!  $m" -ForegroundColor Yellow }

# --- detect architecture ----------------------------------------------------
$arch = 'amd64'
if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') {
  Warn 'Windows ARM64 builds are not published yet — installing the amd64 build (works under emulation).'
}

# --- download + verify + install --------------------------------------------
$url = "https://github.com/$Repo/releases/latest/download/starhold-companion-windows-amd64.zip"
$tmp = Join-Path $env:TEMP ("starhold-install-" + [guid]::NewGuid().ToString('N').Substring(0, 8))
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
  Log "downloading Starhold Companion (windows/$arch)"
  $zipPath = Join-Path $tmp 'comp.zip'
  Invoke-WebRequest -Uri $url -OutFile $zipPath -UseBasicParsing

  # checksum verify against the published checksums.txt
  try {
    $sums = (Invoke-WebRequest -Uri "https://github.com/$Repo/releases/latest/download/checksums.txt" -UseBasicParsing).Content
    $line = ($sums -split "`n") | Where-Object { $_ -match 'starhold-companion-windows-amd64\.zip' } | Select-Object -First 1
    if ($line) {
      $expected = ($line -split '\s+')[0].Trim()
      $actual = (Get-FileHash -Algorithm SHA256 $zipPath).Hash.ToLower()
      if ($actual -ne $expected) { throw "checksum mismatch — aborting (got $actual, want $expected)" }
      Log 'checksum verified'
    }
  } catch {
    Warn "could not verify checksum: $($_.Exception.Message)"
  }

  Expand-Archive -Path $zipPath -DestinationPath $tmp -Force
  $exe = Join-Path $tmp $BinName
  if (-not (Test-Path $exe)) { throw "archive did not contain $BinName" }

  New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
  Copy-Item $exe (Join-Path $InstallDir $BinName) -Force
  Log "installed $(Join-Path $InstallDir $BinName)"

  if (-not (($env:Path -split ';') -contains $InstallDir)) {
    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    [Environment]::SetEnvironmentVariable('Path', "$userPath;$InstallDir", 'User')
    $env:Path += ";$InstallDir"
    Log "added $InstallDir to your user PATH"
  }

  # --- native messaging manifest --------------------------------------------
  $manifest = @{
    name               = $ManifestId
    description        = 'Starhold companion player'
    path               = (Join-Path $InstallDir $BinName)
    type               = 'stdio'
    allowed_origins    = @("chrome-extension://$ExtId/")
    allowed_extensions = @('starhold-companion@starhold.space')
  } | ConvertTo-Json
  $manifestPath = Join-Path $InstallDir "$ManifestId.json"
  $manifest | Out-File -FilePath $manifestPath -Encoding ascii

  # HKCU registry keys (per-user, no admin needed)
  $roots = @(
    'HKCU:\Software\Google\Chrome\NativeMessagingHosts',
    'HKCU:\Software\Microsoft\Edge\NativeMessagingHosts',
    'HKCU:\Software\BraveSoftware\Brave-Browser\NativeMessagingHosts',
    'HKCU:\Software\Mozilla\NativeMessagingHosts'
  )
  foreach ($root in $roots) {
    if (-not (Test-Path $root)) { New-Item -Path $root -Force | Out-Null }
    Set-ItemProperty -Path $root -Name $ManifestId -Value $manifestPath
    Log "registered: $($root.Split('\')[-2])"
  }
} finally {
  Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

# --- what's left -------------------------------------------------------------
Write-Host ''
Write-Host "  Starhold Companion installed."
Write-Host ''
Write-Host '  Remaining steps:'
Write-Host '  1. Browser extension (the manifest ID is already pinned - no editing)'
Write-Host '     - Chrome/Edge/Brave: Menu > Extensions > Manage Extensions >'
Write-Host '       Developer mode > Load unpacked > choose the folder you unzipped'
Write-Host '       starhold-extension-chrome.zip into (zip is on the GitHub releases page)'
Write-Host '     - Firefox: about:debugging > This Firefox > Load Temporary Add-on >'
Write-Host '       pick manifest.json from the unzipped starhold-extension-firefox.zip'
Write-Host '  2. Pair with your server'
Write-Host '     - Starhold web app: Settings > Companion > start pairing,'
Write-Host '       enter the code when the extension asks for it.'
Write-Host ''
Write-Host '  The companion updates itself from GitHub releases automatically'
Write-Host '  (opt out with WYRMHOARD_COMPANION_NO_UPDATE=1).'
