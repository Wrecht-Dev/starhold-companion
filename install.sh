#!/bin/sh
# Starhold Companion — one-shot installer for Linux and macOS.
#
#   curl -fsSL https://raw.githubusercontent.com/Wrecht-Dev/starhold-companion/main/install.sh | sh
#
# What it does:
#   1. downloads the latest release binary for your OS/arch from GitHub
#   2. installs it to ~/.local/bin (created if needed)
#   3. registers the native-messaging manifest for every Chromium/Firefox
#      browser it can find on this machine
#   4. prints the two steps left (extension + pairing)
#
# Safe to re-run (upgrades in place). Requires curl and a shell; no sudo —
# everything installs under your home directory.

set -eu

REPO="Wrecht-Dev/starhold-companion"
BIN_NAME="wyrmhoard-companion"
MANIFEST_ID="cloud.mithraic.wyrmhoard_companion"
VERSION="0.2.0"

log() { printf '\033[1;36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m!!\033[0m %s\n' "$*"; }
die() { printf '\033[1;31mxx\033[0m %s\n' "$*" >&2; exit 1; }

# --- detect platform -------------------------------------------------------
os=$(uname -s)
case "$os" in
  Linux) os_id="linux" ;;
  Darwin) os_id="darwin" ;;
  *) die "unsupported OS: $os (see the README for manual install)" ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch_id="amd64" ;;
  aarch64|arm64) arch_id="arm64" ;;
  *) die "unsupported architecture: $arch" ;;
esac

command -v curl >/dev/null 2>&1 || die "curl is required (install it and re-run)"
command -v tar >/dev/null 2>&1 || die "tar is required"

# --- install location ------------------------------------------------------
BINDIR="${HOME}/.local/bin"
BIN_PATH="${BINDIR}/${BIN_NAME}"
mkdir -p "$BINDIR"

# --- download + verify + install -------------------------------------------
url="https://github.com/${REPO}/releases/latest/download/starhold-companion-${os_id}-${arch_id}.tar.gz"
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

log "downloading Starhold Companion ${VERSION} (${os_id}/${arch_id})"
curl -fsSL "$url" -o "${tmpdir}/comp.tar.gz" || die "download failed — check https://github.com/${REPO}/releases"

# checksum verify against the published checksums.txt
sums_url="https://github.com/${REPO}/releases/latest/download/checksums.txt"
curl -fsSL "$sums_url" -o "${tmpdir}/checksums.txt" || warn "could not fetch checksums.txt (skipping verification)"
if [ -s "${tmpdir}/checksums.txt" ]; then
  expected=$(grep "starhold-companion-${os_id}-${arch_id}.tar.gz" "${tmpdir}/checksums.txt" | awk '{print $1}')
  if [ -n "$expected" ]; then
    if command -v shasum >/dev/null 2>&1; then
      actual=$(shasum -a 256 "${tmpdir}/comp.tar.gz" | awk '{print $1}')
    else
      actual=$(sha256sum "${tmpdir}/comp.tar.gz" | awk '{print $1}')
    fi
    [ "$actual" = "$expected" ] || die "checksum mismatch — aborting (got $actual, want $expected)"
    log "checksum verified"
  fi
fi

tar -xzf "${tmpdir}/comp.tar.gz" -C "$tmpdir"
BIN_SRC="${tmpdir}/${BIN_NAME}"
if [ ! -f "$BIN_SRC" ]; then
  # release archives wrap everything in a top-level directory
  BIN_SRC=$(find "$tmpdir" -name "$BIN_NAME" -type f | head -1)
fi
[ -n "$BIN_SRC" ] && [ -f "$BIN_SRC" ] || die "archive did not contain ${BIN_NAME}"
mv "$BIN_SRC" "$BIN_PATH"
chmod 755 "$BIN_PATH"
log "installed ${BIN_PATH}"

case ":${PATH}:" in
  *":${BINDIR}:"*) ;;
  *) warn "${BINDIR} is not on your PATH — add it:  echo 'export PATH=\"\$HOME/.local/bin:\$PATH\"' >> ~/.profile" ;;
esac

# --- native messaging manifests --------------------------------------------
# The manifest file itself is embedded here so we never need a git checkout.
manifest_tmp="${tmpdir}/${MANIFEST_ID}.json"
cat > "$manifest_tmp" <<EOF
{
  "name": "${MANIFEST_ID}",
  "description": "Starhold Companion — plays media from your Starhold server",
  "path": "${BIN_PATH}",
  "type": "stdio",
  "allowed_origins": ["chrome-extension://okbmiemolnnocdcmohnggigaolobdbef/"],
  "allowed_extensions": ["starhold-companion@starhold.space"]
}
EOF

wrote=0
register_chromium() { # $1 = native hosts dir
  [ -d "$1" ] || mkdir -p "$1" 2>/dev/null || return 1
  cp "$manifest_tmp" "$1/${MANIFEST_ID}.json" 2>/dev/null || return 1
  wrote=$((wrote + 1))
}

for base in "${HOME}/.config" "${HOME}/.var/app"; do
  register_chromium "${base}/google-chrome/NativeMessagingHosts" && log "registered: Google Chrome" || true
  register_chromium "${base}/chromium/NativeMessagingHosts" && log "registered: Chromium" || true
  register_chromium "${base}/BraveSoftware/Brave-Browser/NativeMessagingHosts" && log "registered: Brave" || true
  register_chromium "${base}/microsoft-edge/NativeMessagingHosts" && log "registered: Microsoft Edge" || true
done
# Firefox (per-user)
register_chromium "${HOME}/.mozilla/native-messaging-hosts" && log "registered: Firefox" || true
register_chromium "${HOME}/.librewolf/native-messaging-hosts" && log "registered: LibreWolf" || true

if [ "$wrote" -eq 0 ]; then
  warn "no browser manifest directories could be written — install a browser first, then re-run"
fi

# --- what's left -----------------------------------------------------------
cat <<EOF

  Starhold Companion ${VERSION} installed.

  Remaining steps:

  1. Browser extension (the manifest ID is already pinned — no editing)
     - Chrome/Edge/Brave: Menu → Extensions → Manage Extensions →
       Developer mode → Load unpacked → choose the folder you unzipped
       starhold-extension-chrome.zip into (zip is on the ${REPO}
       releases page)
     - Firefox: about:debugging → This Firefox → Load Temporary Add-on →
       pick manifest.json from the unzipped starhold-extension-firefox.zip

  2. Pair with your server
     - On the Starhold web app: Settings → Companion → start pairing,
       enter the code when the extension asks for it.

  The companion updates itself from GitHub releases automatically
  (opt out with WYRMHOARD_COMPANION_NO_UPDATE=1).

EOF
