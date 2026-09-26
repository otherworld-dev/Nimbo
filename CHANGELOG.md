# Changelog

All notable changes to Nimbo are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [0.2.0] - 2026-09-26

Mostly about file locking and multiple accounts. On-demand mode now takes and
respects locks the way live mode does, locks are let go of when Nimbo quits, and a
colleague opening a document no longer makes everyone else download it again or
end up with a conflicted copy. Each account now gets a folder of its own, large
uploads and downloads no longer hold up the rest of the sync, and there are help
pages linked from the app along with a way to sign in and quit when signed out.

### Added

- In on-demand mode, opening a document in Word or LibreOffice now locks it on the
  server the way live mode does, and a downloaded document a colleague has locked
  opens read-only with their name on it (#7).
- Hovering the tray icon shows what Nimbo is doing ("Syncing…", "Up to date",
  "Paused"), and the icon now has a name in Windows' taskbar settings so it can be
  set to always show (#9).
- While signed out, the flyout can check for updates and turn on beta releases, and
  the tray menu can check for updates too. Before, the only way to update without
  an account was to download the new version from GitHub (#11).
- A crash now leaves a crash.log beside nimbo.log, and it is included in the
  problem report.
- When real-time push isn't available, Settings → Troubleshooting now says it needs
  the Client Push app on the server, links to its setup notes, and says Nimbo checks
  for changes every 30 seconds in the meantime (#13).
- Help pages at nimbosync.com/help, linked from a Help item in the tray menu and
  from Settings, and Report a problem now opens the bug report form directly.
- A lock someone else has left on one of your own files can now be cleared from
  Sync status → In use. The Unlock button only shows for a person's lock that has
  been there for over an hour, and Nimbo checks it is still the same lock before
  clearing it. This helps on hosted servers where occ isn't available and the
  official client has left a lock behind (#7).

### Changed

- Picking Online, Away, Busy or Invisible in the flyout now closes the status
  menu, it only stays open while you are typing a custom status (#7).
- The flyout's recent activity list now scrolls through everything since Nimbo
  started instead of stopping at six items, and has a Clear button. Clearing it
  empties Sync status → Activity as well, anything that still failed stays under
  needs attention (#7).
- Questions and error messages in Settings and Sync status now open in the app's
  own dialog with a proper title, instead of a browser box titled
  "wails.localhost says". They also follow the light or dark theme.

### Fixed

- Files a colleague has open show under In use as soon as Nimbo starts. Before,
  after a restart a lock only reappeared once something else in its folder
  changed, which could take a long time (#7).
- A file someone else had locked no longer stays under In use after its folder
  is deleted on the server. Before, it stayed listed until Nimbo was restarted.
- Two accounts can no longer sync the same folder. A second account was offered the
  first account's folder, and each then uploaded the other's files to its own
  server. Every account now has a folder of its own, and an install already set up
  that way holds the shared folder back, with a notification, until one of the
  accounts is given a different folder (#11).
- Adding an account in on-demand mode no longer turns the other accounts' folders
  into plain copies, and signing out of or removing an account only disconnects
  that account's folder (#11).
- Signing in for the first time in on-demand mode no longer sets up the default
  folder before the setup screen has asked where your files should go. The folder
  you choose is the only one set up, and skipping setup still uses the default
  (#11).
- Leftover Nimbo entries in the Explorer sidebar, from accounts that were switched,
  removed or moved, are cleared up at launch. A synced folder that has been renamed
  or moved is now waited for instead of being created again empty, and a folder on
  a drive that isn't connected is waited for however long it takes (#10).
- The "Show Nimbo in the Explorer sidebar" setting now works in on-demand mode, the
  entry stayed whatever the box said (#7).
- A colleague opening or closing a document no longer makes everyone else download
  it again, or turns an edit made while it was locked into a conflicted copy.
  Locking a file changes its tag on the server without changing its content, and
  both modes now tell the two apart (#7).
- A document open when Nimbo quits, updates or Windows shuts down is now unlocked,
  it used to stay locked for everyone until Nimbo next started. It is locked again
  when Nimbo starts if it is still open, and an unlock that fails is retried every
  30 seconds. Opening one document also sends one lock rather than several, which
  could leave it locked after it was closed.
- Pausing, including "pause for" and quiet hours, now works in on-demand mode.
  Uploads and "Always keep on this device" downloads used to carry on while the app
  said it was paused. Opening a file still downloads it.
- With more than one account, pausing and quiet hours now reach every account
  rather than only the one shown.
- Closing the sign-in window on a fresh install no longer leaves an empty flyout and
  a tray menu that does nothing, both now offer Sign in and Quit (#12).
- An app password the server refuses at launch now asks you to sign in again,
  instead of showing "Can't reach your server" and trying again every 15 seconds
  (#12).
- An Outlook .pst or .ost file is held from the start while Outlook has it open, and
  uploaded once when Outlook lets go of it. Outlook writes to it in bursts that fell
  between uploads, so the whole file was sent again after each one. The flyout
  lists it as waiting rather than as a failed upload.
- Two uploads of the same file no longer run at the same time. They shared one
  upload session on the server and failed with "Chunks on server do not sum up".
- Keep mine and Keep server on a conflict now sync the file with progress and
  retries, instead of doing the whole transfer inside the click with any error only
  in the log. Checking a conflict also uses the server's checksum rather than
  downloading the server copy on every pass.
- Clicking the "Sync conflict" notification opens the Conflicts tab, and the
  "File in use" notification's link works.
- Uploads keep the file's own modified date. The server stamped each file with the
  time it arrived, so other computers downloaded it with the wrong date.
- Switching a live folder to on-demand no longer lists the whole server again when
  the existing sync already knows the folders, and a switch interrupted by closing
  the app finishes its remaining files the next time it starts.
- In on-demand mode, a kept-on-this-device file whose download stalls is retried
  after 90 seconds instead of waiting for good, and a failed download is reported
  once in the activity feed rather than twice.
- A folder with a name the server doesn't allow (such as .htaccess) is now blocked
  like a file would be. Live mode uploaded the folder under a changed name while the
  files inside it still went up under the original one.
- In on-demand mode, a file whose name the server doesn't allow is no longer
  uploaded over a file that already carries its renamed form, and a failed download
  of such a file no longer stays in the Status window after it has synced.
- An account change saved at the same moment as another one (signing in, removing
  an account, changing the default) is no longer lost. Each change read the whole
  account list, edited its own copy and wrote it back, so whichever finished second
  quietly undid the first (#693).
- The flyout names the account it is showing, with its server, on a line of its own
  above the buttons. In Compact width the name used to be cut down to a few letters.
  With more than one account, clicking it lists the accounts to switch to (#14).
- Compact spacing in Settings → Appearance now makes a visible difference: a smaller
  header and rows, and 8 files in Recent activity instead of 6. Before, it changed a
  few pixels of padding. Settings also says what Width and Spacing do (#14).
- In on-demand mode, Settings → Folders no longer says folders aren't used right
  before a list of folders with tick boxes. The list is now called "Keep on this PC
  (offline)", each box says "Keep on this PC", and it explains that ticking a folder
  is the same as Explorer's "Always keep on this device" (#15).
- A large upload or download (64 MB or more) no longer holds up the rest of the
  sync. It now runs alongside the next sync passes, two at a time per account, so a
  new or changed small file goes up without waiting for it to finish. Pausing stops
  them too, and they carry on when the sync resumes (#702).
- Pinned app icons in the dock now load, and apps open on the right page, when
  Nextcloud is installed at a subpath (such as example.com/nextcloud). The subpath
  was being added twice. An icon that still can't load shows the plain app glyph
  instead of a broken image (#16).
- The Start button in the Pin apps editor, and opening an app in its own window,
  now put the app in the Start menu under Nimbo Apps on a new install. Windows was
  keeping the shortcuts in Nimbo's private copy of AppData, where the Start menu
  can't see them. An app already added this way shows as not added, click Start
  again or open the app to add it properly (#16).
- An app's own icon in the Start menu and taskbar is no longer sometimes replaced by
  the Nimbo icon. Two downloads of the same icon could run at once, and the one that
  lost was treated as a failed download (#743).
- In on-demand mode, "Always keep on this device" on a folder now downloads
  everything under it, including subfolders that have never been opened in
  Explorer. Before, only folders that had already been browsed were reached, and
  new files added on the server to a kept folder waited until the next restart to
  download (#17).
- Folders in on-demand mode no longer show the "sync pending" arrows when there is
  nothing waiting to sync. Folders that had never been opened kept them for good,
  and opened folders kept them for up to six hours. Folders now show the cloud
  until their files are downloaded, and ones set up by an earlier version are put
  right a couple of minutes after Nimbo starts (#17).

## [0.1.8] - 2026-09-20

Mostly about deletions, and the ways a folder could be removed when it should not
have been. A thirteen minute server outage was enough for a synced folder to be
read as gone from the server, deleted here, and then sent up as a deletion once
the server came back, so this release closes that off and goes through everything
else that can issue a delete. It also fixes a first sign-in that could fail
outright, along with a batch of upload and status problems found alongside.

### Fixed

- A server outage no longer deletes a synced folder. A folder whose details failed
  to load was read as removed on the server and deleted here, and the deletion was
  then sent up once the server was back.
- In on-demand mode, removing a folder you have never opened (with a script, a
  clean-up tool or rd) no longer deletes it and everything inside it on the server.
  A folder now goes only when this computer has listed what is in it, and otherwise
  it is put back and the activity feed says why.
- A folder deleted on the server that is too big for the Recycle Bin is moved to
  "<folder> - removed on server" beside your sync folder rather than being deleted
  for good. The bin makes room by purging its oldest items, so a folder larger than
  the bin used to lose most of itself without a word.
- A drive whose Recycle Bin capacity cannot be read now moves deletions aside as
  well, instead of counting as a drive with no bin and deleting them for good.
- Deleting a folder clears the records for everything inside it. Those records
  stayed behind saying the files were still synced on both sides, which is what
  later read as a pile of deletions.
- Quitting or pausing partway through a sync no longer marks the folders it never
  reached as up to date, which used to leave files it had not fetched invisible
  until something else in the folder changed.
- Names Nimbo skips on the server (.Trash and similar) are now skipped locally too.
  One that slipped through was downloaded, missed on the next scan, read as deleted
  here, and deleted on the server.
- Unsharing a directly shared file, or a shared folder that had a failure inside it,
  keeps your local copy again instead of sending it to the Recycle Bin (#557).
- Signing in for the first time no longer fails with "the system cannot move the
  file to a different disk drive" on a machine whose AppData folders sit on
  different drives (#8).
- Uploads no longer send torn copies of a file a program is writing to while it is
  read. A large mailbox file open in Outlook was uploaded whole on every session and
  stored under a checksum that did not match it, so every other computer downloaded
  it again and again.
- A file whose copy on the server is damaged is no longer downloaded over and over.
  It is skipped until that copy changes, with one notification saying so, and if you
  have edited the file here the status says the edit is waiting on it.
- Saving in Word, or anything else that saves by replacing the file, is no longer
  blocked while Nimbo is uploading it.
- Files with very long paths (248 characters or more) upload again. The prefix
  Windows needs for them was written incorrectly, so they failed to upload or hash,
  and moving a file into a deep folder was sent up as a deletion with nothing to
  replace it.
- A file whose name contains "401", such as IMG_4012.jpg, no longer signs you out
  when its upload times out. Failures are now told apart by what the server
  returned rather than by searching the message, which also fixes activity entries
  describing the wrong problem.
- Keeping both versions of a conflict fetches the server's copy before moving yours
  aside, so a download that fails no longer leaves nothing under the name.
- A download of a file the server gives no modified time for records the file's own
  time rather than the time it arrived, which used to send it straight back up.
- An upload waiting on a file another program has open says so on the status line,
  is checked every 30 seconds, and goes as soon as the program lets go. It used to
  say "Up to date" and wait for the next hourly pass.
- A local change dropped by a sync that failed during an outage is tried again a
  minute later rather than waiting for the next full pass.
- "Start when I log in" works on packaged builds. The setting wrote a registry value
  the app's container kept private, so Windows never saw it and the app started at
  sign-in whether it was ticked or not.
- Turning the Explorer sidebar entry on and off quickly no longer fails or silently
  drops one of the changes.
- A folder the delete guard puts back appears in Explorer straight away instead of
  needing F5.

### Changed

- "Offline" on the status line now means a network problem only. A folder Windows
  will not let Nimbo read, or a database error, says "Error" instead.
- One path the server keeps refusing no longer holds back syncing from the server.
- Clearing the records for a large deleted folder is much faster. 2,000 deletions
  across a 200,000 row folder took 101 seconds and now takes about two.

## [0.1.7] - 2026-09-18

The first stable release since v0.1.0.277. It brings the on-demand fixes and the
handling of folders that stop being shared from the recent betas to everyone, and
starts the new version numbering, so 0.1.7 is also the number of the next Microsoft
Store release.

### Added

- When a folder stops being shared with you, Nimbo keeps your local copy and parks
  it until you choose to keep it, move it out or delete it (Status → "No longer
  shared"). In on-demand mode the files you had downloaded are kept as well.
- Files set to "Always keep on this device" in on-demand mode are now downloaded,
  including ones that were online-only (#7).
- On-demand files are downloaded through a single streamed request, and a slow
  connection can no longer stall a download past the time limit Windows allows (#7).

### Fixed

- Moving or renaming files and folders in on-demand mode is sent to the server as a
  move, instead of deleting the original and uploading it again, so the file keeps
  its version history.
- A file edited just before a move, or sitting inside a moved folder, keeps its
  pending upload, and a moved file that had unsynced changes is no longer duplicated.
- Checking an online-only file's details no longer downloads the whole file.
- A file that Windows holds but cannot read is reported once, with the steps to
  recover it.
- After switching off on-demand mode, a file whose server copy changed in the
  meantime is downloaded again instead of being reported as a conflict.
- A download that fails part way through is now reported, and one that is
  cancelled is no longer counted as a failure.

### Changed

- Version numbers now follow the release (0.1.7) instead of 0.1.0.<build>. Beta
  builds are numbered on top of the last release, for example 0.1.7.296.
