package vfs

// SyncFirstResult is what Watcher.SyncFirst found to do for a path.
type SyncFirstResult struct {
	// Paused: syncing is paused. Nothing was done.
	Paused bool
	// NotSynced: the path is outside the folder, or carries a name that is
	// never synced (a temporary or lock file).
	NotSynced bool
	// Uploads is how many uploads were started now rather than at their
	// retry time.
	Uploads int
	// Downloads is how many downloads of files kept on this device were
	// moved ahead of the others.
	Downloads int
	// Busy: the file is already being uploaded or downloaded.
	Busy bool
	// OnlineOnly: the file is online-only and in step with the server, so
	// there is nothing waiting to sync. It is not downloaded.
	OnlineOnly bool
}
