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
VERSION="${WYRMHOARD_COMPANION_VERSION:-}"

log() { printf '\033[1;36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m!!\033[0m %s\n' "$*"; }
die() { printf '\033[1;31mxx\033[0m %s\n' "$*" >&2; exit 1; }

# --- resolve version (latest release unless pinned) ------------------------
if [ -z "$VERSION" ]; then
  VERSION=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null | sed -n 's/.*"tag_name": *"v\([^"]*\)".*/\1/p' | head -n1)
fi
[ -n "$VERSION" ] || VERSION="0.2.1"

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

# --- mpv: the actual decoder ------------------------------------------------
# The companion drives an installed mpv. Without it nothing plays. Try the
# native package manager where one exists (needs sudo on most distros).
if ! command -v mpv >/dev/null 2>&1; then
  log "mpv is not installed — it is the actual media decoder"
  installed_mpv=0
  if [ "$os_id" = "linux" ]; then
    if command -v apt-get >/dev/null 2>&1; then
      (sudo apt-get update -qq && sudo apt-get install -y mpv) 2>/dev/null && installed_mpv=1
    elif command -v dnf >/dev/null 2>&1; then
      (sudo dnf install -y mpv) 2>/dev/null && installed_mpv=1
    elif command -v pacman >/dev/null 2>&1; then
      (sudo pacman -S --noconfirm mpv) 2>/dev/null && installed_mpv=1
    elif command -v zypper >/dev/null 2>&1; then
      (sudo zypper install -y mpv) 2>/dev/null && installed_mpv=1
    elif command -v apk >/dev/null 2>&1; then
      (sudo apk add mpv) 2>/dev/null && installed_mpv=1
    fi
  elif [ "$os_id" = "darwin" ]; then
    if command -v brew >/dev/null 2>&1; then
      brew install mpv >/dev/null 2>&1 && installed_mpv=1
    fi
  fi
  if [ "$installed_mpv" = "1" ]; then
    log "mpv installed"
  else
    warn "could not install mpv automatically — install it yourself:"
    if [ "$os_id" = "darwin" ]; then
      warn "  brew install mpv"
    else
      warn "  sudo apt install mpv   (Debian/Ubuntu)"
      warn "  sudo dnf install mpv   (Fedora)"
      warn "  sudo pacman -S mpv     (Arch)"
    fi
    warn "playback will not work until mpv is present."
  fi
else
  log "mpv found: $(command -v mpv)"
fi

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
  *)
    if [ "$os_id" = "darwin" ]; then
      warn "${BINDIR} is not on your PATH — add it:  echo 'export PATH=\"$HOME/.local/bin:$PATH\"' >> ~/.zprofile"
    else
      warn "${BINDIR} is not on your PATH — add it:  echo 'export PATH=\"$HOME/.local/bin:$PATH\"' >> ~/.profile"
    fi ;;
esac

# --- extension identity ----------------------------------------------------
# The ID is derived from the signing key pinned in extension/manifest.json, so
# both the CRX served by the server and the zips share it. The native-messaging
# manifests must allow exactly this origin.
EXT_ID="dgcgodfjhghjbmmgehkgkgnoleacdimf"
SERVER="${STARHOLD_SERVER:-https://starhold.space}"
UPDATE_URL="${SERVER%/}/companion/update.xml"

# --- native messaging manifests --------------------------------------------
# The manifest file itself is embedded here so we never need a git checkout.
manifest_tmp="${tmpdir}/${MANIFEST_ID}.json"
cat > "$manifest_tmp" <<EOF
{
  "name": "${MANIFEST_ID}",
  "description": "Starhold Companion — plays media from your Starhold server",
  "path": "${BIN_PATH}",
  "type": "stdio",
  "allowed_origins": ["chrome-extension://${EXT_ID}/"],
  "allowed_extensions": ["starhold-companion@starhold.space"]
}
EOF

# Per-user host dirs (no privileges needed) and, when we can get sudo, the
# system-wide dirs so an admin-force-installed extension can still reach us.
personal_dirs=""
if [ "$os_id" = "darwin" ]; then
  personal_dirs="${HOME}/Library/Application Support/Google/Chrome/NativeMessagingHosts
${HOME}/Library/Application Support/Chromium/NativeMessagingHosts
${HOME}/Library/Application Support/BraveSoftware/Brave-Browser/NativeMessagingHosts
${HOME}/Library/Application Support/Microsoft Edge/NativeMessagingHosts
${HOME}/Library/Application Support/Mozilla/NativeMessagingHosts"
else
  personal_dirs="${HOME}/.config/google-chrome/NativeMessagingHosts
${HOME}/.config/chromium/NativeMessagingHosts
${HOME}/.config/BraveSoftware/Brave-Browser/NativeMessagingHosts
${HOME}/.config/microsoft-edge/NativeMessagingHosts
${HOME}/.config/vivaldi/NativeMessagingHosts
${HOME}/.var/app/org.chromium.Chromium/config/chromium/NativeMessagingHosts
${HOME}/.mozilla/native-messaging-hosts
${HOME}/.librewolf/native-messaging-hosts"
fi

register_manifest() { # $1 = manifest dir
  mkdir -p "$1" 2>/dev/null || return 1
  cp "$manifest_tmp" "$1/${MANIFEST_ID}.json" 2>/dev/null || return 1
  log "native host registered: ${1}"
}

wrote=0
while IFS= read -r d; do
  [ -n "$d" ] || continue
  register_manifest "$d" && wrote=$((wrote + 1)) || true
done <<EOF
$personal_dirs
EOF

# --- browser extension: zero-click install via enterprise policy -----------
# Chrome-family browsers accept a self-hosted CRX3 when an admin policy forces
# it. Chrome, Chromium, Edge, Brave and Vivaldi all honour this. It needs root
# (Linux policy dirs, macOS managed prefs), so we ask once and degrade cleanly.
policy_json=$(cat <<EOF
{
  "ExtensionInstallForcelist": ["${EXT_ID};${UPDATE_URL}"],
  "ExtensionSettings": {
    "${EXT_ID}": {
      "installation_mode": "force_installed",
      "update_url": "${UPDATE_URL}",
      "toolbar_pin": "force_pinned"
    }
  }
}
EOF
)
policy_tmp="${tmpdir}/starhold-policy.json"
printf '%s\n' "$policy_json" > "$policy_tmp"

have_sudo=0
if [ "$(id -u)" -eq 0 ]; then have_sudo=1
elif command -v sudo >/dev/null 2>&1 && sudo -n true 2>/dev/null; then have_sudo=1
elif command -v sudo >/dev/null 2>&1; then
  log "the browser extension installs system-wide and needs sudo once"
  sudo -v 2>/dev/null && have_sudo=1 || true
fi

policy_written=0
install_policy_dir() { # $1 = managed policy dir
  [ "$have_sudo" = "1" ] || return 1
  sudo mkdir -p "$1" 2>/dev/null || return 1
  sudo cp "$policy_tmp" "$1/starhold-companion.json" 2>/dev/null || return 1
  log "extension policy written: ${1}"
  policy_written=$((policy_written + 1))
}

if [ "$os_id" = "linux" ]; then
  install_policy_dir /etc/opt/chrome/policies/managed || true
  install_policy_dir /etc/chromium/policies/managed || true
  install_policy_dir /etc/opt/edge/policies/managed || true
  install_policy_dir /etc/brave/policies/managed || true
  install_policy_dir /etc/opt/vivaldi/policies/managed || true
  if [ "$have_sudo" = "1" ]; then
    for d in /etc/opt/chrome/native-messaging-hosts /etc/chromium/native-messaging-hosts \
             /etc/opt/edge/native-messaging-hosts /etc/brave/native-messaging-hosts; do
      sudo mkdir -p "$d" 2>/dev/null && sudo cp "$manifest_tmp" "$d/${MANIFEST_ID}.json" 2>/dev/null || true
    done
  fi
else
  # macOS: Chrome-family reads machine-managed preferences from /Library.
  for dom in com.google.Chrome org.chromium.Chromium com.microsoft.Edge com.brave.Browser com.vivaldi.Vivaldi; do
    plist="/Library/Managed Preferences/${dom}.plist"
    if [ "$have_sudo" = "1" ]; then
      plist_tmp="${tmpdir}/${dom}.plist"
      plutil -create xml1 "$plist_tmp" 2>/dev/null && \
        plutil -insert ExtensionInstallForcelist -array "$plist_tmp" 2>/dev/null && \
        plutil -insert ExtensionInstallForcelist.0 -string "${EXT_ID};${UPDATE_URL}" "$plist_tmp" 2>/dev/null && \
        sudo mkdir -p "/Library/Managed Preferences" 2>/dev/null && \
        sudo cp "$plist_tmp" "$plist" 2>/dev/null && \
        { log "extension policy written: ${plist}"; policy_written=$((policy_written + 1)); } || true
    fi
  done
fi

# --- Firefox ---------------------------------------------------------------
# Mozilla force-installs only add-ons signed by addons.mozilla.org, so a
# self-hosted XPI cannot be policy-installed and there is nothing to automate:
# release builds require one confirmation click (see the notes below).
# --- what's left -----------------------------------------------------------
cat <<EOF

  Starhold Companion ${VERSION} installed.
  extension id: ${EXT_ID}

EOF

if [ "$policy_written" -gt 0 ]; then
  cat <<EOF
  The browser extension is installed for you: restart every browser once and it
  appears pinned to the toolbar (Chrome, Chromium, Edge, Brave, Vivaldi).

EOF
else
  cat <<EOF
  Automatic extension install needs admin rights (sudo) to write the browser
  policy. Re-run with sudo, or install it by hand once:
    1. download ${SERVER%/}/companion-extension.zip and unzip it
    2. chrome://extensions -> Developer mode -> Load unpacked -> pick the folder

EOF
fi

if command -v firefox >/dev/null 2>&1; then
  cat <<EOF
  Firefox: release builds only install add-ons signed by Mozilla, so this one
  needs a single confirmation — open about:debugging#/runtime/this-firefox,
  "Load Temporary Add-on", and pick manifest.json from the unzipped
  companion-extension-firefox.zip.

EOF
fi

cat <<EOF
  Pair with your server
  - Open the Starhold web app -> Settings -> Companion -> Start pairing.
  - The extension asks for the code on its own; type it in the browser.
  - Terminal alternative:  wyrmhoard-companion pair ${SERVER} YOUR-CODE

  The companion updates itself from GitHub releases automatically
  (opt out with WYRMHOARD_COMPANION_NO_UPDATE=1).

EOF
