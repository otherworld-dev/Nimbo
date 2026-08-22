package engine

import "os"

// LocalMatchesRemote reports whether a local file already holds the server's
// content, judged on size plus a 2-second mtime window.
//
// The window exists because clients preserve server mtimes with limited
// precision (FAT-era 2s granularity, and the official Nextcloud client rounds
// the same way), so an exact comparison would reject files that are genuinely
// identical. A zero remote mtime is never a match: without a timestamp there is
// nothing to compare, and transferring is cheaper than wrongly assuming the
// local copy is current.
//
// Callers must rule out dehydrated placeholders first (see cfapi.IsDehydrated):
// such a file reports the server's size and mtime while holding no bytes, so it
// would match here despite being empty.
func LocalMatchesRemote(localFI os.FileInfo, r RemoteState) bool {
	if localFI == nil || localFI.IsDir() {
		return false
	}
	if localFI.Size() != r.Size || r.LastModified.IsZero() {
		return false
	}
	dt := localFI.ModTime().Unix() - r.LastModified.Unix()
	return dt >= -2 && dt <= 2
}
