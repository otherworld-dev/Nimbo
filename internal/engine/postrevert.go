package engine

// RestoreInsteadOfDelete rewrites the plan for the first sync pass after a
// folder leaves virtual-files mode: every "delete it on the server" action
// becomes a download, and so does a conflict whose local side is MISSING.
//
// The reconciler infers a server-side delete from "present in the baseline,
// present on the server, missing locally". That inference is right in normal
// operation and WRONG straight after a revert, because leaving on-demand mode
// can legitimately leave files missing locally: a directory whose placeholders
// never populated is empty on disk, and nobody deleted anything.
//
// On 2026-08-16 that cost a shared file: the server's copy was deleted and every
// other machine then removed its local copy as "removed remotely". The
// bulk-delete guard did not fire and could not — it needs 50+ deletions and half
// the pair, and this was one file.
//
// The conflict case is the same inference one step on (Deck #678): an
// online-only stub the revert deleted, whose server copy had ALSO changed since
// the last live baseline, reads as "deleted locally but modified remotely" and
// is handed to the user to resolve — for a file nobody touched locally, and one
// the forecast dialog promised would simply download after the switch. Only a
// conflict whose path is absent on disk qualifies (missingLocally says so); a
// file that IS there was edited on both sides and stays the user's call. A nil
// missingLocally leaves conflicts alone.
//
// Restoring is the safe direction. The worst case is that a file the user really
// did delete before switching comes back, which they can delete again; the
// alternative loses data that only a trash dive recovers, from the FILE OWNER's
// trash if it was shared in.
func RestoreInsteadOfDelete(actions []Action, remote map[string]RemoteState, missingLocally func(path string) bool) (out []Action, restored []string) {
	for _, a := range actions {
		r, onServer := remote[a.Path]
		switch {
		case a.Kind == ActDeleteRemote && onServer:
			if r.IsDir {
				out = append(out, Action{Kind: ActCreateLocalDir, Path: a.Path, Reason: "restoring after leaving virtual files"})
			} else {
				out = append(out, Action{Kind: ActDownload, Path: a.Path, Reason: "restoring after leaving virtual files"})
			}
			restored = append(restored, a.Path)
		case a.Kind == ActConflict && onServer && !r.IsDir && missingLocally != nil && missingLocally(a.Path):
			out = append(out, Action{Kind: ActDownload, Path: a.Path, Reason: "restoring after leaving virtual files (server copy changed meanwhile)"})
			restored = append(restored, a.Path)
		default:
			out = append(out, a) // nothing to restore from, or not ours to decide; leave it alone
		}
	}
	return out, restored
}
