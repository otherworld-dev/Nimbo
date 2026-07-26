package agent

// Windows file attributes that mark a file whose bytes are NOT on this disk.
// They matter during a clone: such a file reports its full logical size and the
// server's mtime, so size/mtime comparison alone would wrongly conclude the
// content is already present. Spelled out here because syscall omits them (only
// golang.org/x/sys/windows defines them) and they are needed on every platform
// for the shared decision logic below.
const (
	fileAttributeOffline            = 0x00001000 // FILE_ATTRIBUTE_OFFLINE
	fileAttributeRecallOnOpen       = 0x00040000 // FILE_ATTRIBUTE_RECALL_ON_OPEN
	fileAttributeRecallOnDataAccess = 0x00400000 // FILE_ATTRIBUTE_RECALL_ON_DATA_ACCESS
)

// placeholderAttrs reports whether a Windows attribute bitmask marks a
// dehydrated file — a cloud placeholder (the official Nextcloud/ownCloud
// client's virtual-files mode, OneDrive, Nimbo's own on-demand mode) or an
// HSM-tiered file, whose contents are fetched on access.
//
// Deliberately NOT included: FILE_ATTRIBUTE_PINNED/UNPINNED (0x80000/0x100000),
// which merely say whether a cloud file is allowed to dehydrate. A hydrated
// cloud file is fully on disk and adopting it is correct. Only the recall/
// offline bits mean "the bytes are elsewhere".
//
// It errs toward true: a false positive costs one re-download, while a false
// negative records a hollow file as synced and it is never repaired.
func placeholderAttrs(attrs uint32) bool {
	const mask = fileAttributeOffline | fileAttributeRecallOnOpen | fileAttributeRecallOnDataAccess
	return attrs&mask != 0
}
