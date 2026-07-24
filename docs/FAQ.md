# Nimbo FAQ

Quick answers to the most common questions. See also
[Troubleshooting](TROUBLESHOOTING.md) and [Known limitations](LIMITATIONS.md).

## What is Nimbo?

A desktop sync client for **Nextcloud** on Windows, built from scratch to be
fast, reliable, and light on resources. It syncs folders two-way, supports
selective sync, virtual (online-only) files, Explorer integration, sharing,
file versions, and real-time updates via your server's `notify_push` service.

## How do I install it?

Two options:

- **Setup.exe** (easiest): download `Nimbo-Setup.exe` from the
  [latest release](https://github.com/otherworld-dev/Nimbo/releases/latest)
  and run it.
- **PowerShell** (App Installer feed — also how updates are delivered):

  ```powershell
  Add-AppxPackage -AppInstallerFile "https://github.com/otherworld-dev/Nimbo/releases/latest/download/Nimbo.appinstaller"
  ```

> **SmartScreen warning?** Nimbo is currently signed with a developer
> certificate rather than a commercial one, so Windows may warn on first
> install. This is expected for now; see [Known limitations](LIMITATIONS.md).

## How do updates work?

Nimbo checks for updates in the background about once a day and shows a
notification when one is available. You can also update on demand:
**Settings → General → About → Check for updates → Update now**. The update
installs in a few seconds and Nimbo restarts itself.

## How do I connect my Nextcloud?

On first launch, enter your server address. Nimbo opens your browser and uses
Nextcloud's standard **Login Flow** — you approve the device in the browser and
never type your password into Nimbo. The resulting app password is stored in
the **Windows Credential Manager**, not in a file.

## Can I use more than one account?

Yes — and they all **sync at the same time** (e.g. a personal and a work
account, each into its own folders). **Settings → General → Account →
＋ Add account** signs in to another server or user. The menus and folder
settings show one account at a time — click **Show** next to another account
to manage that one. Each account keeps its own folder setup and sync state,
and signing out of one hands the app over to the next.

## What's the difference between "Live file system" and "Virtual file system"?

In **Settings → Folders → File availability**:

- **Live file system** (default): the folders you choose are fully downloaded
  and kept in sync on disk. Best for working offline.
- **Virtual file system**: your whole account appears in your sync folder as
  online-only placeholders that download on first open — like OneDrive's
  Files On-Demand. Right-click items in Explorer to keep them offline or free
  up space. This mode is newer than live sync — see
  [Known limitations](LIMITATIONS.md).

## How do I sync only some folders?

Add the folders you want individually (**Settings → Folders → ＋ Add folder**),
or sync everything and then untick subfolders via **Choose folders…** next to
the connection. Unticking never deletes anything on the server; you choose
whether to keep or remove the local copies.

## How do I stop certain files from syncing?

**Settings → Exclusions → Ignore patterns.** Add glob patterns like `*.log`,
`node_modules`, or `build/out`. Common development and OS clutter
(`node_modules`, `.git`, `Thumbs.db`, editor temp files…) is ignored by
default. Ignored items are left untouched on both sides.

## Why won't my `.htaccess` file sync?

Most Nextcloud servers reject web-server configuration files (`.htaccess`,
`.htpasswd`, `.user.ini`), so Nimbo blocks them by default. If your server
accepts them, allow them in **Settings → Exclusions → Allowed filenames**.

## What happens when a file changes in both places?

Nimbo detects the conflict and, by default, asks you what to do. You can change
this under **Settings → Sync → Conflicts** to always keep both versions or
always keep the newest.

## Does Nimbo phone home / collect telemetry?

No. Nimbo talks to **your** Nextcloud server, and to GitHub only to check for
updates. There is no analytics or crash reporting of any kind.

## Where are my logs?

**Settings → General → Troubleshooting → View logs.** Enable **Verbose (debug)
logging** there if you're chasing a problem.

## Is there a CLI?

Yes — `nimbo.exe` ships in the repo build and covers login, sync, pairs,
shares, file operations (`ls`/`get`/`put`/`rm`), ignore rules, and integrity
checks (`nimbo repair`). Run `nimbo help` for the full list.

## How do I report a problem?

Open an issue at <https://github.com/otherworld-dev/Nimbo/issues> with what you
did, what happened, and (if possible) the relevant lines from **View logs**.
See [Troubleshooting](TROUBLESHOOTING.md) first — many issues have quick fixes.
