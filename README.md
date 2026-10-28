# Starhold Companion

A small native-messaging host that lets the Starhold web app play media in an
**installed mpv** on your desktop — no transcoding, no quality loss. The browser
extension talks to this companion over Chrome's native-messaging API; the
companion drives mpv playing the **original file** streamed straight from your
server.

## Why

Browsers can't decode everything (HEVC 10-bit, DTS/TrueHD audio, ...). Rather
than re-encoding on the server, Starhold pairs your browser with a native
player that can decode anything mpv can — which is everything.

## Install

One line per OS. Installs the binary, verifies the download against the
published checksum, and registers the native-messaging manifest for every
browser it finds (Chrome, Chromium, Edge, Brave, Firefox) — no editing, no
admin rights.

**Linux / macOS**

```sh
curl -fsSL https://raw.githubusercontent.com/Wrecht-Dev/starhold-companion/main/install.sh | sh
```

**Windows (PowerShell)**

```powershell
irm https://raw.githubusercontent.com/Wrecht-Dev/starhold-companion/main/install.ps1 | iex
```

**Windows (cmd.exe)**

```bat
powershell -NoProfile -ExecutionPolicy Bypass -Command "irm https://raw.githubusercontent.com/Wrecht-Dev/starhold-companion/main/install.ps1 | iex"
```

Two things to know up front:

- **mpv is the actual decoder.** The installer tries to install it for you
  (needs `sudo` on Linux; `brew` on macOS). If it could not, install it
  yourself — one package, any distro.
- **Naming**: the project is *Starhold*; the binary keeps its historical name
  `wyrmhoard-companion`. Same thing — the binary name predates the rename.
  It is the Starhold Companion.

The installer also sets up the browser extension for you. It writes the
browser's enterprise policy so the extension is **force-installed from a
self-hosted CRX3** (no Web Store listing, no developer mode, no unpacked
folder) and registers the native-messaging host for every browser it finds.

| Browser | Linux | macOS | Windows |
|---|---|---|---|
| Chrome / Chromium | ✅ policy (needs `sudo`) | ✅ managed prefs (needs `sudo`) | ✅ HKCU registry, HKLM when elevated |
| Edge / Brave / Vivaldi | ✅ policy | ✅ managed prefs | ✅ registry |
| Firefox | ⚠️ one click — Mozilla only force-installs AMO-signed add-ons | ⚠️ same | ⚠️ same |
| Safari | ❌ needs a signed app extension (out of scope) | ❌ | — |

Restart each browser once after installing; the extension appears pinned to
the toolbar and the native host is already registered, so **Settings →
Companion → Start pairing** in the web app is the only step left. If the
installer had no admin rights it prints the exact two-line manual fallback.

- The extension id is `dgcgodfjhghjbmmgehkgkgnoleacdimf`, derived from the
  signing key pinned in `extension/manifest.json`. The same key signs the CRX
  served at `https://<your-server>/companion/starhold-extension.crx` with the
  update manifest at `https://<your-server>/companion/update.xml`
  (override the server with `STARHOLD_SERVER=https://your.server`).
- Firefox release builds refuse self-hosted add-ons: the installer prints the
  one-time `about:debugging` step. A permanently signed AMO release removes
  it. Safari needs a notarized app-extension bundle, which this project does
  not ship.
- Both extension zips are also downloadable from your server
  (`/companion-extension.zip`, `/companion-extension-firefox.zip`).

**Pair**: in the Starhold web app open **Settings → Companion** → start
pairing. The extension asks for the code by itself — type it in the browser
and you are done. (Terminal alternative, no angle brackets:
`wyrmhoard-companion pair https://your.server YOUR-CODE`)

Manual install: download a release archive
([Releases](https://github.com/Wrecht-Dev/starhold-companion/releases/latest)),
unpack the binary onto your PATH, and copy
`packaging/cloud.mithraic.wyrmhoard_companion.json` into your browser's
`NativeMessagingHosts` directory with the binary path filled in.

Building the self-hosted extension package yourself:

```sh
# create/lock the signing key once; keep it — the extension id derives from it
go run ./cmd/crx3 -dir extension -key ~/.starhold/ext-signing-key.pem \
  -codebase https://your.server/companion/starhold-extension.crx \
  -crx starhold-extension.crx -xml update.xml
```

## Embedded playback (play in the page, no window)

When the web player cannot decode a file directly, the paired companion can
play it *inside the browser page*: it runs mpv headlessly on your desktop,
decodes the original file, and re-serves it as a fragmented MP4 stream on a
`127.0.0.1`-only local port. The page's own `<video>` element consumes that
stream (MediaSource), so pause/seek/volume/fullscreen all work with the
regular player controls and no second window ever opens.

Safety of the local stream, in brief:
- binds loopback only, on a random port, per session;
- requires a per-session 128-bit URL token that never touches disk;
- answers requests only from your paired server's web origin;
- device token rides IPC-configured headers, never argv or URLs;
- tears down mpv and the listener the moment the tab disconnects.

Chrome shows a one-time "allow local network access" prompt the first time;
after that it is silent.

## Windowed playback

The classic path still works: `play` opens the file in a real mpv window with
the original bytes (zero re-encode). The web player offers whichever mode fits
— in-page when you want it, windowed when you want the full mpv UI.

## Auto-updates

The companion checks GitHub releases once a day (and offers `wyrmhoard-companion
update` to force it). Updates are verified against the published
`checksums.txt` and swapped in atomically; a running mpv is never interrupted —
the new version is picked up the next time the browser starts the host.
Set `WYRMHOARD_COMPANION_NO_UPDATE=1` to pin a version (managed installs).

## Build from source

```sh
go build -o wyrmhoard-companion ./cmd/wyrmhoard-companion
```

Releases are built automatically by GitHub Actions on tagged versions.

## Security model

- No background network listener. The only socket the companion ever opens is
  the loopback relay during embedded playback, gated as described above.
- Only plays media from the paired server URL; arbitrary URLs/files are refused.
- Pairing tokens are device-scoped and revocable from the web app.
