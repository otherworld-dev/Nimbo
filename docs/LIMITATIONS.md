# Known limitations

Honest scope-setting for the current release. Items here are either by design,
on the roadmap, or awaiting upstream pieces. See the [FAQ](FAQ.md) for what
Nimbo *does* do.

## Platforms

- **Windows 10/11 (x64) is the supported desktop platform today.** The GUI is
  built and tested there.
- **Linux:** the sync engine and full CLI already build and run on Linux; the
  GUI port (tray + windows, AppImage packaging) is prepared but not yet
  released. macOS is not currently planned.

## Installation trust

- Releases are signed with a **developer certificate**, not (yet) a
  commercial code-signing certificate, so Windows SmartScreen may warn on
  first install and the App Installer flow requires trusting the certificate
  once. A commercial certificate is planned before wider distribution.

## Virtual file system (on-demand files)

- This mode is **newer than live sync** and should be treated as such: it has
  automated tests and self-heals where it can, but it has seen far fewer
  real-world hours. Switching back to **Live file system** is always available
  and non-destructive.
- On-demand mode applies to the **whole account** (one virtual root), not to
  individual folders.
- Placeholders require NTFS and Windows 10 1709 or newer.

## Accounts

- Multiple accounts **sync side by side** — a personal and a work account both
  stay in sync continuously. The windows, flyout, file search, and folder
  settings show one account at a time (the **shown** account); use
  Settings → General → Account → Show to manage a different one. Background
  accounts surface conflicts and errors via desktop notifications and their
  status line in the account list.
- In **virtual file system** mode every account gets its own on-demand root
  (background accounts mount as "Nimbo - <user>" folders); the
  available-offline panel and Explorer Share/Versions actions operate on the
  shown account's root.

## Server features

- **End-to-end encrypted (E2EE) folders are not supported.** Nimbo doesn't
  implement Nextcloud's E2EE client crypto. It **detects** E2EE folders and
  skips them safely — they're left untouched on the server, excluded from
  virtual-files listings, and you get a one-time notification naming the
  skipped folder. (Regular server-side encryption is fine — it's transparent
  to clients.)
- **Federated/external storage** mounts sync like normal folders but inherit
  their backends' quirks (ETags that change without content changes can cause
  re-downloads).
- Server-side **forbidden filenames** (e.g. `.htaccess`) are blocked client
  side; you can override the list if your server allows them
  (Settings → Exclusions → Allowed filenames).

## Windows specifics

- **File names that are invalid on Windows** (`:` `|` `?` trailing dots/spaces,
  reserved names like `CON`) can exist on the server but cannot be created
  locally; Nimbo reports them as blocked rather than mangling them.
- **Explorer overlay icons** share a system-wide budget with other sync
  clients; if too many are installed, Windows silently drops some.
- Paths longer than ~260 characters work, but some third-party programs
  editing files inside the sync folder may still have trouble with them.

## Operational

- **No telemetry** also means no automatic crash reports — if something
  breaks, we only know if you [tell us](https://github.com/otherworld-dev/Nimbo/issues).
- The update feed is hosted on GitHub; updating requires github.com to be
  reachable.
