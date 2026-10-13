package main

import (
	"log/slog"
	"path/filepath"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/otherworld/nimbo/internal/agent"
	"github.com/otherworld/nimbo/internal/brand"
	"github.com/otherworld/nimbo/internal/notify"
	"github.com/otherworld/nimbo/internal/vfs"
)

// Explorer's "Sync this first" (Deck #702, stage 3). The menu entry runs
// `nimbo-gui --sync-first <path>`, which reaches the running app the way the
// other entries do (onSecondInstance, or pendingSyncFirst when the menu
// started the app). The path is then handed to whatever syncs it:
//
//   - an on-demand folder's watcher (vfs.Watcher.SyncFirst), or
//   - the engine of the account whose live sync folder holds it
//     (agent.Engine.SyncFirst), which moves a large transfer to the front of
//     the queue or syncs the path in a quick pass.
//
// The user is told only when nothing could be started: syncing is paused, the
// path is not synced, or it is outside every sync folder. Otherwise Explorer's
// own sync icon is the feedback, as it is for any other change.

// syncFirstOutcome is how a request ended, as far as the user is concerned.
type syncFirstOutcome int

const (
	sfAsked      syncFirstOutcome = iota // handed over; nothing to say
	sfOutside                            // in no sync folder
	sfPaused                             // syncing is paused
	sfNotSynced                          // left out of syncing
	sfHeld                               // its folder is not syncing; why says why
	sfOnlineOnly                         // online-only and in step: nothing waiting
	sfNotReady                           // its on-demand folder is still being set up
)

// routeSyncFirst hands abs to what syncs it and says how that went.
//
// mounted says abs is inside an on-demand folder, and mount is that folder's
// watcher (nil while the folder is still being set up). On-demand folders are
// looked at first: an account's on-demand folder can also be on record as one
// of its sync folders, which its engine does not sync in that mode. live has
// one entry per running account, tried in turn until one holds the path.
func routeSyncFirst(abs string, paused, mounted bool, mount func(string) vfs.SyncFirstResult,
	live []func(string) agent.SyncFirstResult) (syncFirstOutcome, string) {
	if mounted {
		switch {
		case paused:
			return sfPaused, ""
		case mount == nil:
			return sfNotReady, ""
		}
		r := mount(abs)
		switch {
		case r.Paused:
			return sfPaused, ""
		case r.NotSynced:
			return sfNotSynced, ""
		case r.OnlineOnly:
			return sfOnlineOnly, ""
		}
		return sfAsked, ""
	}
	for _, syncFirst := range live {
		r := syncFirst(abs)
		switch r.Kind {
		case agent.SyncFirstOutside:
			continue // another account's, perhaps
		case agent.SyncFirstPaused:
			return sfPaused, ""
		case agent.SyncFirstNotSynced:
			return sfNotSynced, ""
		case agent.SyncFirstHeld:
			return sfHeld, r.Why
		}
		return sfAsked, "" // asked for, or gone since the menu was opened
	}
	return sfOutside, ""
}

// syncFirstMessage is what to tell the user about an outcome: a toast's title
// and text. Both are empty when there is nothing to say.
func syncFirstMessage(name string, o syncFirstOutcome, why string) (title, text string) {
	switch o {
	case sfPaused:
		return "Syncing is paused", name + " was not synced. Resume syncing, then try again."
	case sfNotSynced:
		return "Not synced", name + " is left out of syncing: it matches an ignore rule, is a temporary file, or is in a folder you chose not to sync."
	case sfHeld:
		return "Folder not syncing", name + " can't sync at the moment. " + why
	case sfOnlineOnly:
		return "Nothing to sync", name + " is online-only and up to date. Choose \"Always keep on this device\" to download it."
	case sfNotReady:
		return "Not ready yet", name + " can't sync yet: its folder is still being set up."
	}
	return "", ""
}

// syncFirstLocalPath handles "Sync this first" for a local file or folder.
// Called on the UI thread, like the other menu entries; the work itself runs
// on a goroutine, because for a folder it walks the folder.
func (a *App) syncFirstLocalPath(localAbs string) {
	if localAbs == "" {
		return
	}
	if a.eng == nil {
		a.queueSyncFirst(localAbs) // the menu started the app: start() hands it back
		return
	}
	localAbs = filepath.Clean(localAbs)
	var mount func(string) vfs.SyncFirstResult
	dir, mounted := a.onDemandMountFor(localAbs)
	if mounted {
		if m := a.onDemandMounts[dir]; m != nil && m.watcher != nil {
			mount = m.watcher.SyncFirst
		}
	}
	var live []func(string) agent.SyncFirstResult
	a.eachEngine(func(e *agent.Engine) { live = append(live, e.SyncFirst) })
	paused := a.Paused()

	go func() {
		outcome, why := routeSyncFirst(localAbs, paused, mounted, mount, live)
		slog.Info("sync this first", "path", localAbs, "outcome", int(outcome))
		name := filepath.Base(localAbs)
		if outcome == sfOutside {
			// A dialog, as the menu's other entries show for the same thing,
			// but one for a whole selection: the menu is offered on every
			// file, and thirty files chosen outside a sync folder asked for
			// thirty dialogs.
			if !a.syncFirstMaySay("outside", time.Now()) {
				return
			}
			application.InvokeAsync(func() {
				if a.app != nil {
					a.app.Dialog.Warning().
						SetTitle("Can't sync this first").
						SetMessage(name + " isn't inside a folder synced by " + brand.Current.Name + ".").
						Show()
				}
			})
			return
		}
		if title, text := syncFirstMessage(name, outcome, why); title != "" && a.syncFirstMaySay(title, time.Now()) {
			notify.Toast(title, text, "")
		}
	}()
}

// syncFirstToastEvery spaces out identical toasts: the menu runs once per
// selected item, so a selection of fifty files asks fifty times in a second.
const syncFirstToastEvery = 5 * time.Second

// syncFirstMaySay reports whether a toast with this title may be shown now,
// and counts it as shown if so.
func (a *App) syncFirstMaySay(title string, now time.Time) bool {
	a.syncFirstMu.Lock()
	defer a.syncFirstMu.Unlock()
	if last, ok := a.syncFirstSaid[title]; ok && now.Sub(last) < syncFirstToastEvery {
		return false
	}
	if a.syncFirstSaid == nil {
		a.syncFirstSaid = map[string]time.Time{}
	}
	a.syncFirstSaid[title] = now
	return true
}

// pendingSyncFirstMax bounds the requests kept while there is no engine.
const pendingSyncFirstMax = 256

// queueSyncFirst keeps a request until the engine is up. Unlike the other
// entries' single pending path this is a list: a selection of several files
// arrives as several requests.
func (a *App) queueSyncFirst(localAbs string) {
	a.syncFirstMu.Lock()
	defer a.syncFirstMu.Unlock()
	if len(a.pendingSyncFirst) < pendingSyncFirstMax {
		a.pendingSyncFirst = append(a.pendingSyncFirst, localAbs)
	}
}

// takePendingSyncFirst returns the queued requests and forgets them.
func (a *App) takePendingSyncFirst() []string {
	a.syncFirstMu.Lock()
	defer a.syncFirstMu.Unlock()
	out := a.pendingSyncFirst
	a.pendingSyncFirst = nil
	return out
}
