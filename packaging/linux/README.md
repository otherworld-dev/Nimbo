# Nimbo on Linux — status & build

Nimbo's core is cross-platform Go and already compiles cleanly for Linux. The CLI
is fully usable today; the GUI builds with the Wails v3 native deps. Windows-only
OS integrations (on-demand files, Explorer overlays/menu) are absent on Linux for
now — handled by no-op stubs so everything still builds and runs.

## Status

| Area | Linux | Notes |
|---|---|---|
| Sync engine, diff, transfer, state | ✅ | Pure Go; `go build ./internal/...` is clean for `GOOS=linux`. |
| WebDAV transport, notify_push, capabilities | ✅ | Pure Go (incl. the push keepalive). |
| File watcher | ✅ | `fsnotify` → inotify on Linux (`watch_fsnotify.go`). |
| Account secrets (keychain) | ✅ | `zalando/go-keyring` → D-Bus **Secret Service** (gnome-keyring / KWallet). Needs a running secret service. |
| Desktop notifications | ✅ | `beeep` → `notify-send`. |
| Autostart at login | ✅ | `~/.config/autostart/nimbo.desktop` (`autostart_linux.go`). |
| CLI (`nimbo`) | ✅ | Login, sync, watch, ls/get/put/rm, repair, share, ignore, … |
| GUI (`nimbo-gui`) | 🟡 | Builds with Wails v3 + GTK4/WebKitGTK 6.0 (checked on Debian 13). Does not run yet, WebKit crashes as the windows load (see below). |
| On-demand / virtual files | ❌ | Windows Cloud Files API only. Linux would need a FUSE/`kio`/`gvfs` approach (future). |
| Explorer overlays + context menu | ❌ | Windows shell extensions. Linux: Nautilus/Dolphin extensions (future). |
| In-place auto-update | ❌→🟡 | The App Installer feed is Windows-only. On Linux use the package manager, AppImage update, or the in-app GitHub check. |

## Building the CLI

Trivial — pure Go, no dependencies:

```bash
CGO_ENABLED=0 go build -o bin/nimbo ./cmd/nimbo
./bin/nimbo login https://your.nextcloud
./bin/nimbo sync ~/Nextcloud
```

## Building the GUI

Wails v3 uses GTK4 + WebKitGTK 6.0 via cgo, so build **on Linux** (no cross-compile
from Windows). Install the native deps first.

**Debian 13 / Ubuntu 24.04+:**
```bash
sudo apt install -y build-essential pkg-config libgtk-4-dev libwebkitgtk-6.0-dev
```
**Fedora:**
```bash
sudo dnf install -y gtk4-devel webkitgtk6.0-devel
```
**Arch:**
```bash
sudo pacman -S gtk4 webkitgtk-6.0
```

Plus Go (1.26+, see `go.mod`) and Node (18+). Then:

```bash
packaging/linux/build.sh      # CLI + frontend + GUI -> bin/
./bin/nimbo-gui
```

Older distros without WebKitGTK 6.0 can build against GTK3 + WebKit2GTK 4.1
instead (`libgtk-3-dev libwebkit2gtk-4.1-dev`) by adding `-tags gtk3` to the GUI's
`go build`. The tray talks to the StatusNotifier service over D-Bus, so no
appindicator library is needed to build.

**It builds but does not run yet.** With Wails v3.0.0-alpha.96 both builds crash
in WebKit while the app's windows load their pages: the GTK4 build in
`soup_message_headers_iter_next` (the response headers of the first request),
the GTK3 build in `webkit_uri_scheme_request_get_http_body` or
`soup_message_headers_new` as a second window loads (also on a real Ubuntu 22.04
desktop, not only in a container). Nimbo serves its
frontend with Wails' stock `AssetFileServerFS`, so this looks like a Wails/WebKit
problem, but it has not been tried with a bare Wails app yet.

## Known Linux work items

- **App icon:** the GUI embeds `assets/nimbo.ico` (Windows). Add a PNG and an
  `.desktop` for proper launcher/window-icon integration.
- **Relaunch:** `relaunchSelf` is a no-op off Windows (`restart_other.go`); a
  Linux re-exec would be needed for an in-app "restart" (only used by update
  flows, which differ on Linux anyway).
- **Tray:** verify the Wails v3 system-tray works across GNOME (needs the
  AppIndicator extension), KDE, and XFCE.
- **Packaging:** ship as **AppImage** (self-contained, good for "download &
  run"), **Flatpak** (sandboxed, Flathub distribution), and/or **.deb/.rpm**.
  These replace the Windows MSIX/App Installer pipeline.
- **Updates:** wire the in-app "Check for updates" (already GitHub-backed) to the
  Linux artifact, or rely on the chosen package manager / AppImageUpdate.

## What deliberately won't be on Linux (for now)

On-demand/virtual files, Explorer status-icon overlays, and the right-click
"Share/keep/free" menu are Windows shell integrations. They're stubbed off
Windows so the app builds and runs; Linux file-manager equivalents are a separate
effort. Everything else — real two-way sync, selective sync, conflicts, versions,
trash, sharing, notifications, presence — is platform-neutral.
