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
| GUI (`nimbo-gui`) | 🟡 | Builds with Wails v3 + GTK3/WebKit2GTK; tray via libayatana-appindicator. Needs real-world testing. |
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

Wails v3 uses GTK3 + WebKit2GTK via cgo, so build **on Linux** (no cross-compile
from Windows). Install the native deps first.

**Debian/Ubuntu:**
```bash
sudo apt install -y build-essential pkg-config \
  libgtk-3-dev libwebkit2gtk-4.1-dev libayatana-appindicator3-dev
```
**Fedora:**
```bash
sudo dnf install -y gtk3-devel webkit2gtk4.1-devel libappindicator-gtk3-devel
```
**Arch:**
```bash
sudo pacman -S gtk3 webkit2gtk-4.1 libayatana-appindicator
```

Plus Go (1.22+) and Node (18+). Then:

```bash
packaging/linux/build.sh      # CLI + frontend + GUI -> bin/
./bin/nimbo-gui
```

(If `webkit2gtk-4.1` isn't available, the 4.0 series works too — adjust the dev
package name. Wails v3's required versions are in its docs.)

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
