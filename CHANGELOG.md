# Changelog

All notable changes to Nimbo are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

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
