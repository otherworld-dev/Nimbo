// Package engine contains Nimbo's sync logic: the data model for the
// three observed states (remote, local, baseline), discovery of the remote and
// local trees, and the pure three-way diff that reconciles them into a plan of
// actions. It deliberately has no dependency on transport or storage details so
// the reconciliation logic stays easy to test in isolation.
//
// The pure three-way diff depends on nothing but the three state structs below,
// which is what keeps it easy to test. Discovery does touch transport, mostly to
// flatten its Entry into these primitives.
//
// Note: this lives in package "engine" rather than "sync" to avoid shadowing
// the standard library's sync package.
package engine

import (
	"time"

	"github.com/otherworld/nimbo/internal/transport"
)

// RemoteState is what a PROPFIND told us about a path right now.
type RemoteState struct {
	Path         string // files-root-relative, "/" separators, no leading slash
	IsDir        bool
	ETag         string
	FileID       string // oc:fileid — stable across renames/moves
	Size         int64
	SHA1         string    // content SHA1 from oc:checksums, when the server provides it
	LastModified time.Time // server mtime; populated where needed (e.g. takeover adoption)
	UploadTime   int64     // nc:upload_time (unix seconds), 0 = not reported; see ContentKey
	ReadOnly     bool      // server marks this not-writable (oc:permissions) -> mirror as a local read-only attribute
	// MountRoot marks the top of a share received from someone else, or of an
	// external-storage / group-folder mount: the one node whose disappearance
	// from a listing means "detached from this account", not "deleted". Derived
	// from oc:permissions at scan time (on a mount, parent not) and carried into
	// the baseline so KeepDetached can recognise it after it has gone.
	MountRoot bool
	// Lock is files_lock state, when the server has the app. nil is ambiguous on
	// its own — see LockKnown.
	Lock *transport.LockInfo
	// LockKnown says whether Lock is authoritative: true when this entry came
	// from a real listing or Stat, false when it was replayed from the baseline
	// for a subtree the ETag prune skipped (addBaselineSubtree), which has no
	// Entry behind it and therefore cannot know. Without this, "unlocked" and
	// "we did not look" are indistinguishable, and a pass would silently clear
	// locks in folders it never examined.
	LockKnown bool
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
	// ContentKey is the server version's content key at last sync
	// (transport.ContentKey), "" when unknown. It lets the diff tell a
	// files_lock ETag bump from an edit on files with no checksum.
	ContentKey string
	MountRoot  bool // it was the root of a received share or a mount (see RemoteState.MountRoot)
}

// ContentKey is the listed version's content key ("" for a directory or when
// the server did not report an upload time); see transport.ContentKey.
func (r RemoteState) ContentKey() string {
	if r.IsDir {
		return ""
	}
	return transport.ContentKey(r.Size, r.LastModified, r.UploadTime)
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
