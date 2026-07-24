# Troubleshooting Nimbo

Work top-to-bottom: most problems fall out of the first two sections. See also
the [FAQ](FAQ.md) and [Known limitations](LIMITATIONS.md).

## First steps for any problem

1. **Check Connection health** — Settings → General → Troubleshooting. It shows
   the server, account, real-time push state, and the last sync result.
2. **Look at the logs** — **View logs** in the same section. Turn on
   **Verbose (debug) logging**, reproduce the problem, and look at the newest
   lines.
3. **Restart Nimbo** — tray icon → right-click → Quit (or the ⋯ menu in the
   flyout), then start it again. Sync state is durable; restarting is always
   safe.

## Sync isn't happening

- **Paused?** The flyout shows a pause banner; quiet hours
  (Settings → Sync → Quiet hours) also pause syncing on a schedule.
- **Signed in?** If the tray icon shows the warning badge and the flyout says
  "Sign in again", your app password was revoked — sign in again.
- **Push "reconnecting…"** in Connection health is fine for a moment after the
  network changes; sync still works via periodic polling without push. If it
  never connects, your server may not have the `notify_push` app — that's OK,
  just slower to notice remote changes.
- **One file refuses to sync?** Check the flyout's error list. Common causes:
  the name is invalid on Windows (`:`/`|`/trailing dot), the server forbids it
  (`.htaccess` — see the FAQ), or it matches an ignore pattern
  (Settings → Exclusions).

## Conflicts

When a file changed in both places, Nimbo (by default) asks. If you chose
"keep both", the second version is saved next to the original as a conflict
copy with a timestamp in its name. Change the default under
Settings → Sync → Conflicts.

## Updates

- **"Update now" shows an error** — it tells you why (usually the network or
  GitHub being briefly rate-limited). Try again in a few minutes.
- **Stuck on an old version** — install the latest manually; this also repairs
  update tracking:

  ```powershell
  Add-AppxPackage -AppInstallerFile "https://github.com/otherworld-dev/Nimbo/releases/latest/download/Nimbo.appinstaller" -ForceTargetApplicationShutdown
  ```

- The updater writes `nimbo-update.log` in your user folder; if an update
  failed, the reason is in there.

## Virtual file system (on-demand) issues

- **Folder shows placeholders but opening a file fails** — check Connection
  health; hydration needs the server to be reachable.
- **"The cloud file metadata is corrupt and unreadable"** when deleting or
  opening — a placeholder folder was left behind without its sync provider
  (e.g. after an unclean uninstall). Fix: switch **File availability** back to
  on-demand so Nimbo reconnects the provider, then (if you want rid of the
  folder) switch to Live and let Nimbo clean up. Your files are safe on the
  server either way.
- **Explorer feels slow in the sync folder** — first-time browsing populates
  folders on demand; it's fast after the first visit.
- Virtual files are newer than live sync — if something looks wrong, grab
  verbose logs and report it. Switching back to **Live file system** is always
  available and non-destructive.

## Explorer integration

- **Overlay icons missing** — Windows limits overlay slots system-wide and
  other sync apps (OneDrive, Dropbox) compete for them. Sign out of unused
  ones, or live without overlays — sync itself is unaffected.
- **"Share with Nimbo" missing from the right-click menu** — toggle it off and
  on under Settings → General → System integration, then restart Explorer.

## Sign-in problems

- The browser opens but approval never lands in Nimbo: your server URL may be
  behind a proxy that blocks the login-flow polling. Check
  `https://<server>/status.php` loads in a browser from the same machine.
- Credentials are stored in Windows Credential Manager under `nimbo`. Removing
  the entry and signing in again is a clean reset for auth issues.

## Something looks wrong in the synced data

Run an integrity check from the CLI:

```powershell
nimbo repair          # report differences between local, server, and the baseline
```

It reports (and can fix) divergences without guessing — nothing is changed
unless you confirm.

## Reset, the safe way

To start a machine's setup over **without touching the server**: Settings →
General → Account → Sign out, and tick **"Also remove this device's sync setup
& database"**. Local files stay where they are; a later login starts clean.

> Never delete the local sync folder while signed in with the folder still
> configured — a fresh login afterwards could read the missing files as
> deletions. The tick-box reset above exists exactly to avoid this.

## Reporting a bug

Open an issue at <https://github.com/otherworld-dev/Nimbo/issues> and include:

1. Nimbo version (Settings → General → About) and Windows version,
2. what you did, what you expected, what happened,
3. relevant lines from **View logs** (verbose if possible),
4. for update problems: `nimbo-update.log` from your user folder.
