package main

import "log/slog"

// cleanupStatusRoots unregisters any live-pair status root left behind by
// v0.1.0.245-247, which auto-registered every live pair as a status-only
// cloud sync root to feed the Explorer Status column. The verdict on the
// result (2026-08-20, tried live on real data): a live folder gains the
// Status column, a nav-pane entry, and native "sync pending" arrows on every
// plain item — when all the user wants there is corner badges on the icons.
// So live mode registers NOTHING (badges come from the classic HKLM overlay
// handlers, Setup.exe's elevated step), the Status column stays a
// virtual-files-mode thing, and this heals installs that briefly did
// register. Unregistration is idempotent and cheap when nothing is there.
func (a *App) cleanupStatusRoots() {
	if a.eng == nil {
		return
	}
	pairs, err := a.eng.Pairs()
	if err != nil {
		slog.Warn("status-root cleanup: could not list pairs", "err", err)
		return
	}
	for _, p := range pairs {
		if _, mounted := a.onDemandMounts[p.LocalDir]; mounted {
			continue // a real on-demand root, not ours to touch
		}
		a.eng.DisableStatusIcons(p.LocalDir)
	}
}
