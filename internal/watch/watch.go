// Package watch drives continuous sync: a platform watcher reports changed local
// paths, an external trigger (notify_push) and a poll interval round it out, and
// all sources are debounced and funnelled into a single serialized sync callback
// so runs never overlap. The watcher is recursive ReadDirectoryChangesW on
// Windows (one handle for the whole tree) and fsnotify elsewhere.
package watch

import (
	"context"
	"log/slog"
	"time"
)

// Options configures the watch loop.
type Options struct {
	Root         string        // local directory to watch (recursively)
	PollInterval time.Duration // how often to poll the remote (0 disables)
	Debounce     time.Duration // quiet period after activity before syncing
	// External, if non-nil, is an additional trigger source (e.g. a notify_push
	// "notify_file" event). A receive arms a debounced sync; the push carries no
	// path, so it runs OnPush (a remote-delta reconcile) when set, else a full sync.
	External <-chan struct{}
	// OnPush, if non-nil, handles an External (push) trigger instead of a full
	// SyncFunc pass. A push only means "the server changed", so this reconciles the
	// remote delta without the full local walk. The poll uses it too (see below).
	OnPush func(ctx context.Context) error
	// FullSyncEvery bounds how often the poll does a full local-walking sync. Most
	// polls run the fast OnPush remote-delta (the watcher already covers live local
	// edits); a full pass runs only this often, as a safety net for local changes
	// the watcher missed — so a fast push isn't stuck behind a ~30s walk every poll.
	// Zero defaults to 1 hour. Ignored when OnPush is nil (every poll is then full).
	FullSyncEvery time.Duration
	// FullSync, if non-nil, forces a full local-walking pass on the next fire — for
	// changes that reclassify LOCAL files (name allow-list / escape-list), which a
	// remote-delta trigger wouldn't pick up.
	FullSync <-chan struct{}
}

// SyncFunc performs one reconciliation. It is always called serially. changed
// lists the absolute local paths touched since the previous run (for a scoped
// sync); it is nil for a full sync — on startup, on the poll, and after a remote
// push — so callers should treat nil as "reconcile everything".
type SyncFunc func(ctx context.Context, changed []string) error

// overflowSignal is sent on the events channel by a platform watcher when its
// change buffer overflowed and the specific changed paths were lost. runLoop
// responds with a prompt full local scan to recover them, rather than waiting for
// the (now hourly) periodic full pass.
const overflowSignal = "\x00watch-overflow"

// runLoop debounces changed paths from events plus poll/external triggers and
// dispatches syncs serially, until ctx is cancelled. It runs one full sync on
// start. A push (External) forces the next pass to be full. Platform watchers
// feed events with changed absolute paths.
func runLoop(ctx context.Context, opts Options, sync SyncFunc, events <-chan string) error {
	if opts.Debounce <= 0 {
		opts.Debounce = 2 * time.Second
	}
	if opts.FullSyncEvery <= 0 {
		opts.FullSyncEvery = time.Hour
	}
	// Consecutive pass failures gate the poll/push retries with exponential
	// backoff. Without it a failing pass is re-attempted at full poll cadence —
	// and when the failure is the server buckling under the scan itself, that
	// relentless retry is a DoS loop that never lets it recover (observed in the
	// field: a cold crawl re-fired every 5 minutes for days). Local edits are
	// still synced during backoff (scoped and cheap); only the remote-walking
	// poll/push triggers wait.
	var failStreak int
	var holdUntil time.Time
	base := opts.PollInterval
	if base <= 0 {
		base = 5 * time.Minute
	}
	note := func(err error) {
		if err == nil || ctx.Err() != nil {
			failStreak = 0
			holdUntil = time.Time{}
			return
		}
		failStreak++
		d := base << uint(failStreak-1)
		if d > time.Hour {
			d = time.Hour
		}
		holdUntil = time.Now().Add(d)
		slog.Warn("sync failing; backing off", "failures", failStreak, "retry_in", d)
	}
	holding := func() bool { return failStreak > 0 && time.Now().Before(holdUntil) }

	runSync := func(reason string, changed []string) {
		slog.Info("sync triggered", "reason", reason)
		err := sync(ctx, changed)
		if err != nil && ctx.Err() == nil {
			slog.Error("sync failed", "err", err)
		}
		note(err)
	}
	runPush := func(reason string) {
		slog.Info("sync triggered", "reason", reason)
		err := opts.OnPush(ctx)
		if err != nil && ctx.Err() == nil {
			slog.Error("remote-delta sync failed", "reason", reason, "err", err)
		}
		note(err)
	}

	// Initial reconciliation so we start from a consistent state.
	runSync("startup", nil)
	lastFull := time.Now() // last full local-walking sync (startup counts)

	var poll <-chan time.Time
	if opts.PollInterval > 0 {
		t := time.NewTicker(opts.PollInterval)
		defer t.Stop()
		poll = t.C
	}

	var debounce <-chan time.Time        // nil until armed by an event
	changed := make(map[string]struct{}) // local paths touched since the last fire
	pushed := false                      // a push (no path) is pending for this window
	forceFull := false                   // watcher overflowed → recover with a full scan
	for {
		select {
		case <-ctx.Done():
			return nil

		case p := <-events:
			if p == overflowSignal {
				forceFull = true // lost the specific paths — full scan needed
			} else {
				changed[p] = struct{}{}
			}
			debounce = time.After(opts.Debounce)

		case <-opts.External:
			pushed = true // remote push: reconcile the server delta on the next fire
			debounce = time.After(opts.Debounce)

		case <-opts.FullSync:
			forceFull = true // name-rule change: full local rescan, not a remote delta
			debounce = time.After(opts.Debounce)

		case <-debounce:
			debounce = nil
			paths := make([]string, 0, len(changed))
			for p := range changed {
				paths = append(paths, p)
			}
			changed = make(map[string]struct{})
			doPush := pushed
			pushed = false

			// A watcher overflow lost the specific paths, so do a full local scan to
			// recover — it covers the changed paths and the remote, so skip the rest.
			if forceFull {
				forceFull = false
				runSync("overflow", nil)
				lastFull = time.Now()
				break
			}

			// Local changes carry their paths (scoped/targeted reconcile). A push
			// reconciles the remote delta separately — both can fire in one window.
			if len(paths) > 0 {
				runSync("change", paths)
			}
			if doPush {
				if holding() {
					slog.Debug("push skipped; backing off after failures", "until", holdUntil)
				} else if opts.OnPush != nil {
					runPush("push")
				} else {
					runSync("push", nil) // no delta handler → full pass
				}
			}

		case <-poll:
			// Most polls run the fast remote-delta (the watcher already covers live
			// local edits), so a push isn't stuck behind a ~30s walk. A full pass —
			// the safety net for local changes the watcher missed — runs only every
			// FullSyncEvery.
			if holding() {
				slog.Debug("poll skipped; backing off after failures", "until", holdUntil)
			} else if opts.OnPush != nil && time.Since(lastFull) < opts.FullSyncEvery {
				runPush("poll")
			} else {
				runSync("poll", nil)
				lastFull = time.Now()
			}
		}
	}
}
