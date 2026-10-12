package vfs

// StubFate is what live sync will do with an online-only stub once the switch
// away from virtual files is made. The leaving-VFS scan asks its caller for
// one per stub, so the confirm dialog can say what really happens to each.
//
// Declared in a file with no build tag: the GUI builds the classifier on every
// OS, while the scan that calls it is Windows-only.
type StubFate int

const (
	// StubDownloads: a sync folder covers the path and its rules keep it, so
	// live sync brings the file back as a real file.
	StubDownloads StubFate = iota
	// StubIgnored: a sync folder covers the path, but the ignore rules or that
	// folder's excluded subfolders skip it (Deck #512).
	StubIgnored
	// StubUnsynced: no sync folder covers the path at all, so nothing syncs it
	// (Deck #948).
	StubUnsynced
)
