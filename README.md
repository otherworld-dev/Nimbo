# Nimbo

A from-scratch, cross-platform Nextcloud client in Go, focused on doing the
things the official desktop client does poorly: **reliable sync & conflict
handling**, **selective/smart sync**, **real-time app notifications**, and **low
resource use**.

It talks to Nextcloud entirely over documented HTTP APIs:

- **Files** — WebDAV (`/remote.php/dav/files/<user>/`), using ETags for change
  detection, `oc:fileid` for move detection, and `OC-Checksum` for integrity.
- **Large uploads** — chunked upload v2 (`/remote.php/dav/uploads/...`).
- **Real-time** — the `notify_push` websocket for instant sync triggers and app
  notifications, with OCS polling as a fallback.

## Support

- [Report an issue](https://github.com/otherworld-dev/Nimbo/issues)

## Architecture

A headless **sync daemon** holds all the logic; thin frontends (the CLI and the
Wails tray GUI) drive it over a local control API.

```
cmd/nimbo      command-line interface
cmd/nimbo-gui  system-tray app
internal/account    Login Flow v2, app-password storage (OS keychain), metadata
internal/transport  authenticated HTTP, WebDAV, OCS, capabilities, notifications
internal/engine     three-way diff, remote/local discovery, rename detection
internal/transfer   resumable download, chunked upload, executor, conflict resolution
internal/state      SQLite sync baseline (pure-Go modernc driver)
internal/watch      fsnotify + poll loop with debounce
internal/push       notify_push WebSocket client
internal/notify     OCS notifications → native desktop toasts
internal/activity   live activity/error log for the GUI
internal/agent      reusable engine that drives sync/watch for the CLI and GUI
internal/config     per-OS directories, account + sync-pair persistence
internal/cli        CLI command implementations
```

## Status

**Feature-complete desktop client.** Auth, transport, the sync engine,
bidirectional `sync`, live `watch` with conflict/rename handling, **real-time
push + desktop notifications**, sharing, selective sync, bandwidth limits, and a
**native tray GUI** with first-run sign-in, a tray menu, an activity/conflicts
status window, and a Sync-settings window for choosing folders. Working today:

```sh
go build ./...

# Authenticate (opens a browser; stores an app password in your OS keychain)
nimbo login https://cloud.example.com

nimbo accounts          # list configured accounts
nimbo caps              # server version + notify_push availability
nimbo ls [remote-path]  # list a remote directory
nimbo get <remote> [local]
nimbo put <local> <remote>
nimbo rm  <remote-path>

# The sync engine
nimbo plan  <local-dir> [remote-dir]  # dry run: show what a sync would do
nimbo sync  <local-dir> [remote-dir]  # apply it once (bidirectional)
nimbo watch <local-dir> [remote-dir]  # keep in sync continuously (live)

# Notifications
nimbo notifications     # list current Nextcloud notifications

# Sharing
nimbo share link <remote> [password]   # create a public link
nimbo share user <remote> <user>       # share with a user
nimbo share list <remote> / rm <id>

# Bandwidth
nimbo limit up <kbps> down <kbps>       # caps (KiB/s); 'none' to clear

# Sync pairs (used by the tray app)
nimbo pair add <local-dir> [remote-dir]   # configure a folder to keep synced
nimbo pair list
nimbo pair rm <index>
nimbo pair exclude <index> add <glob>     # per-folder selective sync
nimbo ignore add <glob>                   # global ignore patterns

nimbo logout            # remove account + keychain secret
```

## Desktop GUI (`nimbo-gui`)

A native [Fyne](https://fyne.io) tray app that runs the sync agent in the
background. Set folders up once (`nimbo pair add`), then launch it (e.g. at
login):

- **Tray menu** — quick access to your **Nextcloud apps**, **favourited folders**
  (open locally if synced, else in the browser), and **local synced folders**,
  plus **Open Nimbo / Sync now / Pause / Quit**.
- **Status window** — live tabs for **Activity**, **Notifications** (Accept/
  Decline/Open/Dismiss), **Conflicts** (choose **Keep mine / Keep server / Keep
  both / Open folder** per conflict), **Can't sync**, and **Issues** (Retry).
- **Sync settings** — a tabbed window to:
  - **Folders**: browse your whole account and **tick which folders to sync**
    (each becomes a sync pair under a configurable local base folder); changes
    apply live. **Share** any file/folder from here too.
  - **Bandwidth**: up/down rate limits.
  - **Ignore**: the global ignore-pattern editor.
- **Notifications** — real-time app notifications as native toasts (clickable on
  Windows — opens the relevant link), a tray **Notifications (N)** quick-view with
  dismiss-all, and the full panel above.
- **Can't-sync handling** — files the server forbids (read from its capabilities:
  reserved names, illegal characters/extensions, `.htaccess`, …) are never retried
  into failure. They're flagged in a **Can't sync** tab where you can **Rename**
  them to an allowed name or **Blacklist** them (never attempt again).

### Building the GUI

The GUI is a **Wails v3** app (Go + a Svelte/WebView2 frontend) — it gives a
OneDrive-style tray flyout. It needs Node/npm (frontend build), a C compiler
(MinGW-w64, for cgo), and the WebView2 runtime (ships with Windows 11). The
`nimbo` CLI stays cgo-free and needs none of this.

```sh
# CLI — no C compiler / Node needed
CGO_ENABLED=0 go build -o bin/nimbo ./cmd/nimbo

# GUI:
#   1) build the frontend
cd cmd/nimbo-gui/frontend && npm install && npm run build && cd -
#   2) build the binary (gcc on PATH, cgo on). -H windowsgui = no console
#      window (also stops a console flashing when Explorer invokes a verb).
CGO_ENABLED=1 go build -ldflags "-H windowsgui" -o bin/nimbo-gui ./cmd/nimbo-gui
# (or use the Wails CLI: `wails3 build` for an icon + installer)
```

The engine does a three-way merge (baseline vs. live remote vs. local):
new/changed files transfer in the right direction, deletions propagate, and
unchanged trees are skipped via ETag pruning. Downloads are resumable, written
atomically, and checksum-verified; large uploads use chunked v2. The baseline is
scoped to each **local↔remote pair**, so binding a new local folder to an
existing remote path downloads rather than mistaking absent files for deletions.

Beyond plain sync, it handles the cases the official client struggles with:

- **Renames/moves are cheap.** A file renamed on the server (matched by
  `oc:fileid`) or locally (matched by content hash) becomes a metadata move on
  both ends — no re-download or re-upload, even for huge files.
- **Conflicts are resolved sanely.** If both sides changed to the *same* bytes
  it's silently merged; if they truly diverge, both versions are kept (the local
  one as `name (conflicted copy <timestamp>).ext`); and a delete on one side
  versus an edit on the other keeps the edit (deletion never silently wins).
  In the **GUI** these are presented for you to decide (keep mine / server /
  both); the **CLI** resolves them automatically as above.
- **Ignore rules & selective sync.** Global glob patterns and per-folder excludes
  keep chosen files/subtrees out of sync entirely (gitignore-style).
- **Bandwidth limits & retries.** Optional up/down rate caps, and interrupted
  transfers resume and retry with backoff rather than failing outright.
- **`watch` is live and real-time.** A local filesystem watcher (fsnotify) syncs
  local changes within ~2s. When the server has the `notify_push` app, remote
  changes and app notifications arrive instantly over a WebSocket (with a slow
  poll as a safety net); without it, the client falls back to frequent polling.
- **App notifications as desktop toasts.** Shares, mentions, and app messages pop
  as native notifications (deduplicated), driven by the same push channel.

Secrets are stored in the OS keychain (Windows Credential Manager / macOS
Keychain / freedesktop Secret Service), never on disk.

## Roadmap

See `docs`/the design plan: Phase 2 state DB + three-way diff, Phase 3 transfer
engine (resume + chunked upload), Phase 4 real-time sync + conflict/move
handling, Phase 5 notifications + systray, Phase 6 selective sync + GUI,
Phase 7 on-demand virtual files. **Next: Linux desktop support.**

## License

[PolyForm Noncommercial 1.0.0](LICENSE) — the source is available to read,
build, and use for any noncommercial purpose; commercial use and resale need a
licence from Otherworld Dev Ltd (this keeps a paid business/white-label tier
possible while personal use stays free forever). The app binary is free to
download and use personally either way.

Commercial licensing (team deployments, hosting providers, white-label): see
[nimbosync.com/business](https://www.nimbosync.com/business.html) or email
contact@otherworld.dev.

## Contributing

Bug reports and feature requests are very welcome on the
[issue tracker](https://github.com/otherworld-dev/Nimbo/issues). Code
contributions aren't accepted at the moment — keeping sole copyright is what
makes the free-personal / paid-commercial licensing work. (If that ever
changes, a CLA will come first.)

The **Nimbo** name and icon aren't covered by the licence — don't ship rebuilds
under the same name.

Nimbo is an independent client, not affiliated with or endorsed by
Nextcloud GmbH.
