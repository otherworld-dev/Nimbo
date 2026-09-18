# Changelog

All notable changes to Nimbo are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

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
