package agent

// State-reset tripwire (added after the 2026-07-24 incident, where a Windows
// package repair silently replaced the packaged app's state DB and the engine
// quietly re-cloned everything via takeover). The engine records each pair's
// completed initial clone in a small config-side file — deliberately a
// different directory from the sync database, so losing one doesn't lose the
// other. When a pair is about to be cloned from scratch yet that history says
// it has synced before, the state DB has evidently vanished: the takeover
// clone is still the correct recovery (adopt matching files, never overwrite,
// conflict the rest), so the engine proceeds — but says so out loud instead
// of hiding it.

import "log/slog"

// markPairSynced records that a pair completed its initial clone. Best-effort:
// the marker only powers the state-loss warning. The engine-level lock
// serializes the file's read-modify-write across concurrently-syncing pairs
// (cross-process writers don't exist: dev/CLI builds use their own -dev dir).
func (e *Engine) markPairSynced(pk string) {
	e.histMu.Lock()
	defer e.histMu.Unlock()
	e.markPairSyncedLocked(pk)
}

// markPairSyncedOnce backfills the marker for a pair that is ALREADY synced —
// pairs whose clone finished before this feature existed would otherwise
// never be recorded, and the tripwire would miss their first state loss (the
// only one that matters). Called from the steady-state sync path, so after
// the first call per pair per run it costs a map lookup, not file I/O.
func (e *Engine) markPairSyncedOnce(pk string) {
	e.histMu.Lock()
	defer e.histMu.Unlock()
	if e.histMarked[pk] {
		return
	}
	e.markPairSyncedLocked(pk)
}

func (e *Engine) markPairSyncedLocked(pk string) {
	if err := e.dirs.MarkPairSynced(pk); err != nil {
		slog.Debug("sync-history marker write failed", "err", err)
		return
	}
	if e.histMarked == nil {
		e.histMarked = make(map[string]bool)
	}
	e.histMarked[pk] = true
}

// warnIfStateReset is called when a pair is entering an initial clone (no
// clone state, empty baseline). If the config-side history says this pair
// completed a clone before, warn — log plus a once-per-run toast — because
// the sync database has evidently been lost or reset.
func (e *Engine) warnIfStateReset(pk string, p Pair) {
	hist, err := e.dirs.LoadSyncHistory()
	if err != nil || !hist[pk] {
		return
	}
	slog.Warn("sync state missing for a previously synced pair — recovering via takeover",
		"local", p.LocalDir, "remote", p.RemoteRoot)
	e.stateResetToast.Do(func() {
		e.toast("Nimbo — sync state was reset",
			"Nimbo's sync database was missing, so it is re-checking your files against the server. Your files are safe; anything that changed on only one side in the meantime will appear as a conflict copy.", "")
	})
}
