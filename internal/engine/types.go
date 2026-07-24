// Package engine contains Nimbo's sync logic: the data model for the
// three observed states (remote, local, baseline), discovery of the remote and
// local trees, and the pure three-way diff that reconciles them into a plan of
// actions. It deliberately has no dependency on transport or storage details so
// the reconciliation logic stays easy to test in isolation.
//
// Note: this lives in package "engine" rather than "sync" to avoid shadowing
// the standard library's sync package.
package engine

import "time"

// RemoteState is what a PROPFIND told us about a path right now.
type RemoteState struct {
	Path         string // files-root-relative, "/" separators, no leading slash
	IsDir        bool
	ETag         string
	FileID       string // oc:fileid — stable across renames/moves
	Size         int64
	SHA1         string    // content SHA1 from oc:checksums, when the server provides it
	LastModified time.Time // server mtime; populated where needed (e.g. takeover adoption)
	ReadOnly     bool      // server marks this not-writable (oc:permissions) -> mirror as a local read-only attribute
}

// LocalState is what the filesystem walk told us about a path right now.
type LocalState struct {
	Path  string
	IsDir bool
	Size  int64
	MTime time.Time
}

// BaselineState is the last-known-synced state of a path, persisted between
// runs. It is the common ancestor in the three-way merge: comparing remote and
// local against it tells us which side(s) changed.
type BaselineState struct {
	Path            string
	IsDir           bool
	RemoteETag      string
	RemoteFileID    string
	LocalSize       int64
	LocalMTimeNanos int64
	ContentSHA1     string // SHA1 of the content at last sync; enables move detection
}

// ActionKind enumerates the reconciliation operations the diff can emit.
type ActionKind int

const (
	ActNoop ActionKind = iota
	ActDownload
	ActUpload
	ActCreateLocalDir
	ActCreateRemoteDir
	ActDeleteLocal
	ActDeleteRemote
	ActConflict
	ActMoveLocal  // a remote rename: move the local file from Path to Dest
	ActMoveRemote // a local rename: move the remote file from Path to Dest
)

// String renders an ActionKind as a short, stable label.
func (k ActionKind) String() string {
	switch k {
	case ActNoop:
		return "noop"
	case ActDownload:
		return "download"
	case ActUpload:
		return "upload"
	case ActCreateLocalDir:
		return "mkdir-local"
	case ActCreateRemoteDir:
		return "mkdir-remote"
	case ActDeleteLocal:
		return "delete-local"
	case ActDeleteRemote:
		return "delete-remote"
	case ActConflict:
		return "conflict"
	case ActMoveLocal:
		return "move-local"
	case ActMoveRemote:
		return "move-remote"
	default:
		return "unknown"
	}
}

// Action is a single planned operation. For most kinds only Path is set; move
// actions also set Dest (the new path). Reason is a human-readable explanation.
type Action struct {
	Kind   ActionKind
	Path   string
	Dest   string
	Reason string
}
