#!/bin/sh
# Installs the native-messaging manifest for Chrome/Chromium (per-user) and the
# companion binary. Run from the repository root after `go build`.
set -eu
here=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
bin="${HOME}/.local/bin/wyrmhoard-companion"
mkdir -p "${HOME}/.local/bin"
if [ -x "${here}/wyrmhoard-companion" ]; then
  cp "${here}/wyrmhoard-companion" "$bin"
else
  (cd "$here" && go build -o wyrmhoard-companion ./cmd/wyrmhoard-companion)
  cp "${here}/wyrmhoard-companion" "$bin"
fi
chmod 755 "$bin"
for dir in "${HOME}/.config/google-chrome/NativeMessagingHosts" \
           "${HOME}/.config/chromium/NativeMessagingHosts" \
           "${HOME}/.mozilla/native-messaging-hosts"; do
  mkdir -p "$dir"
  sed "s#/usr/local/bin/wyrmhoard-companion#${bin}#" "${here}/packaging/cloud.mithraic.wyrmhoard_companion.json" > "$dir/cloud.mithraic.wyrmhoard_companion.json"
done
cat > "${HOME}/.config/autostart/wyrmhoard-companion.desktop" <<DESK
[Desktop Entry]
Type=Application
Name=Starhold Companion
Exec=${bin} --version
X-GNOME-Autostart-enabled=false
DESK
echo "installed $bin and native-messaging manifests"
echo "Set the extension ID in the manifests, then load companion/extension unpacked."
