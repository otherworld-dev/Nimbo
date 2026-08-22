package engine

// FilterLocked splits a plan into the actions that may proceed and the uploads
// that must wait because somebody else holds the file locked on the server.
//
// Only UPLOADS are held. A download of a locked file is harmless — it is how
// this machine learns what the lock holder saved — and deletes and directory
// work are unrelated to the contents somebody is editing.
//
// Holding rather than uploading is what makes the lock holder keep the
// filename: if we uploaded now, THEY would get the conflict when they saved,
// having done nothing wrong. Waiting means the conflict, if there even is one,
// lands on the person who edited a file they were told was in use.
//
// A path with unknown lock state (LockKnown false — a subtree the ETag prune
// skipped) is never held: silence is not evidence of a lock.
func FilterLocked(actions []Action, remote map[string]RemoteState, login string) (kept []Action, held []string) {
	for _, a := range actions {
		if a.Kind != ActUpload {
			kept = append(kept, a)
			continue
		}
		r, ok := remote[a.Path]
		if !ok || !r.LockKnown || !r.Lock.HeldByOther(login) {
			kept = append(kept, a)
			continue
		}
		held = append(held, a.Path)
	}
	return kept, held
}
