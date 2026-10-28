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

1. Grab the release for your OS from
   [Releases](https://github.com/Wrecht-Dev/starhold-companion/releases/latest):
   - **Linux**: `starhold-companion-linux-amd64.tar.gz` (also arm64)
   - **Windows**: `starhold-companion-windows-amd64.zip`
   - **macOS**: `starhold-companion-darwin-amd64.tar.gz` (Apple Silicon: arm64)
   - **Browser extension**: `starhold-extension-chrome.zip` (Chrome/Chromium/Edge)
     or `starhold-extension-firefox.zip` (Firefox 109+)
2. Unpack it somewhere on your PATH (Linux/macOS: `~/.local/bin` works well).
3. Install the browser extension for your browser:
   - Chrome/Chromium/Edge: unzip `starhold-extension-chrome.zip`, enable
     developer mode in `chrome://extensions`, click "Load unpacked".
   - Firefox: open `about:debugging#/runtime/this-firefox`, "Load Temporary
     Add-on…", pick any file inside the unzipped `starhold-extension-firefox`
     folder (permanent install lands with AMO signing).
4. In the Starhold web app open **Settings → Companion player** and pair —
   the app shows a pairing code; run
   `wyrmhoard-companion pair <server> <code>` to finish.

### Linux one-step

```sh
tar xzf starhold-companion-linux-amd64.tar.gz && cd starhold-companion-*
sudo ./packaging/install-linux.sh   # or run without sudo for per-user install
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

- No local network listener; all traffic is stdio native-messaging + direct
  HTTPS to your Starhold server.
- Only plays media from the paired server URL; arbitrary URLs/files are refused.
- Pairing tokens are device-scoped and revocable from the web app.
