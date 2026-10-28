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
$ExtId = 'dgcgodfjhghjbmmgehkgkgnoleacdimf'
$Server = if ($env:STARHOLD_SERVER) { $env:STARHOLD_SERVER.TrimEnd('/') } else { 'https://starhold.space' }
$UpdateUrl = "$Server/companion/update.xml"
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

  $roots = @(
    'HKCU:\Software\Google\Chrome\NativeMessagingHosts',
    'HKCU:\Software\Chromium\NativeMessagingHosts',
    'HKCU:\Software\Microsoft\Edge\NativeMessagingHosts',
    'HKCU:\Software\BraveSoftware\Brave-Browser\NativeMessagingHosts',
    'HKCU:\Software\Vivaldi\NativeMessagingHosts',
    'HKCU:\Software\Mozilla\NativeMessagingHosts'
  )
  foreach ($root in $roots) {
    if (-not (Test-Path $root)) { New-Item -Path $root -Force | Out-Null }
    Set-ItemProperty -Path $root -Name $ManifestId -Value $manifestPath
    Log "native host registered: $root"
  }

  # --- browser extension: zero-click install via enterprise policy ----------
  # Chrome-family browsers honour a self-hosted CRX3 when a policy forces it.
  # HKCU policies work without admin for Chrome/Edge/Brave on Windows; HKLM
  # (system-wide) is written too when the shell is elevated.
  $policyValue = "$ExtId;$UpdateUrl"
  $isAdmin = ([Security.Principal.WindowsPrincipal] [Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole(
    [Security.Principal.WindowsBuiltInRole]::Administrator)
  $browserPolRoots = @(
    'Software\Policies\Google\Chrome',
    'Software\Policies\Chromium',
    'Software\Policies\Microsoft\Edge',
    'Software\Policies\BraveSoftware\Brave',
    'Software\Policies\Vivaldi'
  )
  $policyTargets = @('HKCU:') + @(if ($isAdmin) { 'HKLM:' } else { @() })
  $policyWritten = 0
  foreach ($hive in $policyTargets) {
    foreach ($root in $browserPolRoots) {
      $path = "$hive\$root\ExtensionInstallForcelist"
      try {
        if (-not (Test-Path $path)) { New-Item -Path $path -Force | Out-Null }
        Set-ItemProperty -Path $path -Name '1' -Value $policyValue
        $policyWritten++
      } catch { Warn "could not write policy $path : $($_.Exception.Message)" }
    }
  }
  if ($policyWritten -gt 0) { Log "extension policy written for $policyWritten browser policy keys ($ExtId)" }
  if (-not $isAdmin) {
    Warn 'run the installer from an elevated PowerShell to also cover HKLM (other Windows users)'
  }
} finally {
  Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

# --- Firefox ---------------------------------------------------------------
# Mozilla only force-installs add-ons signed by addons.mozilla.org, so a
# self-hosted XPI cannot be policy-installed. Temporary add-on is one click.
$firefox = Get-Command firefox -ErrorAction SilentlyContinue
if ($firefox) {
  Warn 'Firefox: release builds need one confirmation — about:debugging#/runtime/this-firefox -> Load Temporary Add-on -> manifest.json from companion-extension-firefox.zip'
}

# --- what's left -----------------------------------------------------------
Write-Host ''
Write-Host "  Starhold Companion installed.  extension id: $ExtId"
Write-Host ''
if ($policyWritten -gt 0) {
  Write-Host '  The browser extension is installed for you: restart every browser once'
  Write-Host '  and it appears pinned to the toolbar (Chrome, Edge, Brave, Chromium).'
} else {
  Write-Host '  Could not write the browser policy - install the extension by hand once:'
  Write-Host "    $Server/companion-extension.zip  ->  unzip  ->  chrome://extensions  ->"
  Write-Host '    Developer mode  ->  Load unpacked'
}
Write-Host ''
Write-Host '  Pair with your server'
Write-Host '  - Starhold web app: Settings > Companion > start pairing'
Write-Host "  - or:  $InstallDir\\wyrmhoard-companion.exe pair $Server YOUR-CODE"
Write-Host ''
Write-Host '  The companion updates itself from GitHub releases automatically'
Write-Host '  (opt out with WYRMHOARD_COMPANION_NO_UPDATE=1).'
