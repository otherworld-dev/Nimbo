package agent

// The damage guard: refusing a sync pass that looks like the SERVER lost data
// rather than the user changing things.
//
// Nimbo already refuses to propagate a bulk deletion TO the server (see
// localRootVanished and bulkDeleteGuardTrips in agent.go). This is the mirror
// image, and it protects the copy on this PC: a server restored from an old
// snapshot, a shared folder someone emptied, or ransomware.
//
// Two rules the rest of this file exists to keep true:
//
//   - It counts files being REPLACED as well as deleted. Server-side ransomware
//     encrypts in place rather than deleting, so it produces a plan that is all
//     downloads; a deletions-only guard waves every encrypted file through.
//   - A trip refuses the WHOLE pass, downloads included. Halting only the
//     deletions would pull the encrypted copies down over the good ones.
//
// A freeze lives in the account's guard-state file rather than the state
// database, so clearing the sync database cannot silently unfreeze a folder — a
// re-clone is exactly what would overwrite what the freeze was protecting.

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/otherworld/nimbo/internal/config"
	"github.com/otherworld/nimbo/internal/engine"
	"github.com/otherworld/nimbo/internal/state"
	"github.com/otherworld/nimbo/internal/syncguard"
)

// errGuardStateUnavailable is what every pair on the account fails with while
// the guard state cannot be read.
//
// Refusing is the safe direction: the file records which folders are FROZEN, so
// reading it as "nothing is frozen" would resume a folder the guard stopped and
// apply the very changes it was holding back.
func errGuardStateUnavailable() error {
	return fmt.Errorf("the sync guard state is unreadable — syncing is suspended for this " +
		"account, because a paused folder would otherwise resume and apply the " +
		"changes the guard stopped")
}

// reloadGuardState re-reads the account's guard state. On a READ failure it
// stores nil rather than an empty set, and nil reads as unavailable.
func (e *Engine) reloadGuardState() error {
	set, err := e.dirs.LoadGuardState()
	if err != nil {
		slog.Error("guard state unreadable — suspending sync for this account so a paused "+
			"folder cannot silently resume", "err", err)
		e.guard.Store(nil)
		return errGuardStateUnavailable()
	}
	e.guard.Store(&set)
	return nil
}

// guardStateUnavailable reports that the guard state could not be read, so no
// pair on this account may sync.
//
// The zero value of the field is nil, so an Engine that never loaded its state
// reads as unavailable. A construction path that forgets to load it fails
// loudly rather than quietly running every folder unguarded.
func (e *Engine) guardStateUnavailable() bool { return e.guard.Load() == nil }

// guardFor returns a pair's guard state, and whether it has any. Most pairs have
// none — an entry is written only when a folder is frozen.
func (e *Engine) guardFor(p Pair) (config.GuardState, bool) {
	set := e.guard.Load()
	if set == nil {
		return config.GuardState{}, false
	}
	f, ok := (*set)[PairKey(p.LocalDir, p.RemoteRoot)]
	return f, ok
}

// sweepGuardState drops entries whose sync pair no longer exists. Called once at
// Run, before any watcher exists, so nothing is mid-pass.
//
// config.LoadPairs returns a nil slice on every error path, so the length check
// alone would currently catch a read failure too — mutating away either check on
// its own leaves the tests green. Both are kept because that overlap is an
// accident of LoadPairs' current contract rather than a guarantee, and because
// the two states want different log lines when this goes wrong on a user's
// machine.
func (e *Engine) sweepGuardState() {
	set := e.guard.Load()
	if set == nil || len(*set) == 0 {
		return // unreadable (the account is suspended), or nothing that could be stale
	}
	pairs, err := e.Pairs()
	if err != nil {
		slog.Warn("guard sweep: cannot list the sync folders — leaving the state alone", "err", err)
		return
	}
	if len(pairs) == 0 {
		return
	}
	s, err := e.dirs.LoadAccountState()
	if err != nil {
		slog.Warn("guard sweep: cannot read the account state, so cannot tell which folders are "+
			"parked — leaving the state alone", "err", err)
		return
	}
	live := make(map[string]bool, len(pairs)+len(s.RememberedPairs))
	for _, sp := range pairs {
		live[PairKey(sp.LocalDir, sp.RemoteRoot)] = true
	}
	for _, sp := range s.RememberedPairs {
		live[PairKey(sp.LocalDir, sp.RemoteRoot)] = true
	}

	dropped := 0
	if err := e.dirs.UpdateGuardState(func(cur config.GuardStates) {
		for pk := range cur {
			if !live[pk] {
				delete(cur, pk)
				dropped++
			}
		}
	}); err != nil {
		slog.Warn("guard sweep: could not drop the entries whose folder is gone", "err", err)
		return
	}
	if dropped == 0 {
		return
	}
	slog.Info("guard sweep: dropped entries whose sync folder is gone", "count", dropped)
	_ = e.reloadGuardState()
}

// frozenPairErr is the refusal for a frozen folder, or nil.
//
// Checked at ensurePair as well as applyPlan, because the initial clone never
// reaches applyPlan: SyncOnce routes a pair whose clone status is not "done"
// straight into cloneRemote. A freeze lives outside the state database precisely
// so it survives a reset — and a reset is exactly what puts a frozen pair back
// into that state, where a re-clone would download whatever the server now holds
// over the copy the freeze was protecting.
func (e *Engine) frozenPairErr(p Pair) error {
	f, ok := e.guardFor(p)
	if !ok || f.Frozen == nil {
		return nil
	}
	return fmt.Errorf("syncing %q is paused pending review: %s", p.LocalDir, f.Frozen.Reason)
}

// freezePair records a tripped guard and surfaces it.
//
// It CREATES the entry when there is none. Presence in this file says nothing
// about how a folder syncs — it only records that a folder is frozen — so any
// pair can acquire one, which is the whole point of guarding every folder.
func (e *Engine) freezePair(p Pair, f config.Freeze) error {
	pk := PairKey(p.LocalDir, p.RemoteRoot)
	if err := e.dirs.UpdateGuardState(func(s config.GuardStates) {
		cur := s[pk]
		cur.Frozen = &f
		cur.ExemptNext = false
		s[pk] = cur
	}); err != nil {
		return err
	}
	_ = e.reloadGuardState()
	e.status("Sync paused — needs review")
	e.toastFrozen(p.LocalDir, f.Reason)
	slog.Error("damage guard: pausing folder", "local", p.LocalDir, "reason", f.Reason)
	return nil
}

// toastFrozen surfaces a freeze to the desktop.
//
// Deliberately NOT toastGuardTripped: that one's wording ("your sync folder is
// missing or empty") describes the server-protecting guard, and would send the
// user hunting for a missing folder when the folder is fine and the SERVER lost
// files. It shares the same throttle, so both tripping within five minutes still
// produces one notification.
func (e *Engine) toastFrozen(local, reason string) {
	if e.onToast == nil {
		return
	}
	e.toastMu.Lock()
	throttled := time.Since(e.lastErrToast) < 5*time.Minute
	if !throttled {
		e.lastErrToast = time.Now()
	}
	e.toastMu.Unlock()
	if throttled {
		return
	}
	e.toast("Nimbo — sync paused",
		"“"+filepath.Base(local)+"”: "+reason+". Nothing on this PC was changed. "+
			"Review it in Settings and resume if this was expected.", "")
}

// clearFreeze resumes a frozen folder, granting a single-pass exemption so the
// very run the user approved is not refused again.
func (e *Engine) clearFreeze(p Pair) error {
	pk := PairKey(p.LocalDir, p.RemoteRoot)
	if err := e.dirs.UpdateGuardState(func(s config.GuardStates) {
		cur, ok := s[pk]
		if !ok {
			return // nothing frozen: nothing to resume
		}
		cur.Frozen = nil
		cur.ExemptNext = true
		s[pk] = cur
	}); err != nil {
		return err
	}
	return e.reloadGuardState()
}

// consumeGuardExemption clears a one-pass exemption, reporting whether one was
// held.
//
// The ExemptNext check reads the LOADED set; the write re-reads the file. An
// entry that has gone in between reports NOT held — the same safe direction as a
// write we could not make.
func (e *Engine) consumeGuardExemption(p Pair) bool {
	f, ok := e.guardFor(p)
	if !ok || !f.ExemptNext {
		return false
	}
	pk := PairKey(p.LocalDir, p.RemoteRoot)
	spent := false
	if err := e.dirs.UpdateGuardState(func(s config.GuardStates) {
		cur, ok := s[pk]
		if !ok {
			return
		}
		cur.ExemptNext = false
		s[pk] = cur
		spent = true
	}); err != nil {
		// Report NOT held: a reprieve we cannot record is one that would be
		// granted again on every pass, permanently disabling the guard.
		slog.Warn("could not spend the guard exemption — guarding this pass", "err", err)
		return false
	}
	_ = e.reloadGuardState()
	return spent
}

// baselineForGuard returns a baseline map that covers every path in the plan.
//
// applyPlan's base argument is the map the plan was diffed against, EXCEPT on
// the SyncPaths route, which passes nil on purpose: its remote map is built from
// individual stats rather than a listing, and a non-nil base there would stamp
// directory ETags from a scan that never happened.
//
// A nil baseline is not a harmless "no rows" for the guard: syncguard.Count only
// scores a download as an OVERWRITE when the path has a baseline row, so a nil
// one turns a total rewrite of a folder into a pass of pure "additions" and the
// guard waves it through. Failure is fatal to the pass; guessing "not tracked"
// is what produces that hole.
func (e *Engine) baselineForGuard(
	st *state.Store,
	pk string,
	base map[string]engine.BaselineState,
	actions []engine.Action,
) (map[string]engine.BaselineState, error) {
	if base != nil || len(actions) == 0 {
		return base, nil
	}
	paths := make([]string, 0, len(actions)*2)
	for _, a := range actions {
		paths = append(paths, a.Path)
		if a.Dest != "" {
			paths = append(paths, a.Dest)
		}
	}
	rows, err := st.LoadBaselinePaths(pk, paths)
	if err != nil {
		return nil, fmt.Errorf("guard: cannot tell tracked files from new ones: %w", err)
	}
	return rows, nil
}

// tripGuard freezes a pair and returns the error that aborts the pass.
func (e *Engine) tripGuard(p Pair, reason string, c syncguard.Counts, known int) error {
	f := config.Freeze{
		AtUnix:     time.Now().Unix(),
		Deletions:  c.Deletions,
		Overwrites: c.Overwrites,
		Known:      known,
		Reason:     reason,
		Sample:     c.Sample,
	}
	if err := e.freezePair(p, f); err != nil {
		slog.Error("could not record the freeze", "err", err)
	}
	return fmt.Errorf("sync paused for %q: %s — nothing was downloaded or removed. "+
		"Review it in Settings and resume if this was expected", p.LocalDir, reason)
}

// --- the GUI-facing layer ---------------------------------------------------
//
// The GUI knows a folder by its LOCAL DIRECTORY — what the Settings list shows
// and what PairDTO carries — while everything above is keyed by PairKey.

// FrozenView is one folder's guard state, flattened for the GUI.
type FrozenView struct {
	Frozen       bool
	FreezeReason string
	FreezeSample []string
}

// FrozenViews returns guard state keyed by local directory, for GetPairs.
//
// It reads the loaded set rather than the file: an unreadable state stores nil,
// so this reports nothing frozen while the account is suspended. The engine
// refuses to sync at all in that state, so the UI cannot mislead anyone into
// thinking a paused folder is running.
func (e *Engine) FrozenViews() map[string]FrozenView {
	out := map[string]FrozenView{}
	set := e.guard.Load()
	if set == nil {
		return out
	}
	pairs, err := e.Pairs()
	if err != nil {
		return out
	}
	for _, sp := range pairs {
		f, ok := (*set)[PairKey(sp.LocalDir, sp.RemoteRoot)]
		if !ok || f.Frozen == nil {
			continue
		}
		out[sp.LocalDir] = FrozenView{
			Frozen:       true,
			FreezeReason: f.Frozen.Reason,
			FreezeSample: f.Frozen.Sample,
		}
	}
	return out
}

// ClearFreeze resumes a frozen folder by local directory.
func (e *Engine) ClearFreeze(localDir string) error {
	sp, err := e.pairAt(localDir)
	if err != nil {
		return err
	}
	p := Pair{LocalDir: sp.LocalDir, RemoteRoot: sp.RemoteRoot}
	if err := e.frozenPairErr(p); err == nil {
		return fmt.Errorf("%q is not paused, so there is nothing to resume", sp.LocalDir)
	}
	slog.Info("freeze cleared by the user", "local", sp.LocalDir)
	return e.clearFreeze(p)
}

// ForgetSyncFolder is the USER removing a connection: the pair goes, and its
// guard state goes with it.
//
// The order and the gate both matter. The entry is keyed to the pair, so it can
// only be dropped once the pair has actually gone — if the removal FAILS the
// folder is still configured and still watched, and dropping its entry there
// would resume a folder the guard had paused.
//
// Distinct from Engine.RemoveSyncFolder, which is also how pairs are cleared
// TEMPORARILY (the on-demand switch remembers and restores them) — that path
// must leave the guard state alone.
func (e *Engine) ForgetSyncFolder(remoteRoot string, deleteLocal bool) error {
	// Read the pair first: once it is removed there is nothing left to derive
	// its PairKey from.
	var sp config.SyncPair
	found := false
	if pairs, err := e.Pairs(); err == nil {
		for _, p := range pairs {
			if strings.Trim(p.RemoteRoot, "/") == strings.Trim(remoteRoot, "/") {
				sp, found = p, true
				break
			}
		}
	}
	if err := e.RemoveSyncFolder(remoteRoot, deleteLocal); err != nil {
		return err // the folder is still synced — its freeze still applies
	}
	if !found {
		return nil
	}
	return e.ForgetGuardState(sp.LocalDir, sp.RemoteRoot)
}

// ForgetGuardState drops a pair's guard entry, for a folder that has gone away.
// Callers must have removed the pair FIRST — see ForgetSyncFolder.
func (e *Engine) ForgetGuardState(localDir, remoteRoot string) error {
	pk := PairKey(localDir, remoteRoot)
	if err := e.dirs.UpdateGuardState(func(s config.GuardStates) { delete(s, pk) }); err != nil {
		return err
	}
	return e.reloadGuardState()
}

// pairAt finds the configured pair for a local directory, comparing paths the
// way the rest of config does (cleaned and case-folded) rather than literally.
func (e *Engine) pairAt(localDir string) (config.SyncPair, error) {
	pairs, err := e.Pairs()
	if err != nil {
		return config.SyncPair{}, err
	}
	want := config.PathKey(localDir)
	for _, sp := range pairs {
		if config.PathKey(sp.LocalDir) == want {
			return sp, nil
		}
	}
	return config.SyncPair{}, fmt.Errorf("no sync folder at %q", localDir)
}
