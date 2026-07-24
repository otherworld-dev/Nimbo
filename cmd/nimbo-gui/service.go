package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"github.com/otherworld/nimbo/internal/account"
	"github.com/otherworld/nimbo/internal/activity"
	"github.com/otherworld/nimbo/internal/agent"
	"github.com/otherworld/nimbo/internal/applog"
	"github.com/otherworld/nimbo/internal/cfapi"
	"github.com/otherworld/nimbo/internal/autostart"
	"github.com/otherworld/nimbo/internal/brand"
	"github.com/otherworld/nimbo/internal/config"
	"github.com/otherworld/nimbo/internal/license"
	"github.com/otherworld/nimbo/internal/notify"
	"github.com/otherworld/nimbo/internal/policy"
	"github.com/otherworld/nimbo/internal/overlay"
	"github.com/otherworld/nimbo/internal/shellmenu"
	"github.com/otherworld/nimbo/internal/shellns"
	"github.com/otherworld/nimbo/internal/transfer"
	"github.com/otherworld/nimbo/internal/transport"
	"github.com/otherworld/nimbo/internal/update"
	"github.com/otherworld/nimbo/internal/vfs"
)

// App is the Wails-bound service: its exported methods are callable from the
// frontend, and it forwards engine changes to the UI as events.
type App struct {
	app         *application.App
	ctx         context.Context
	eng         *agent.Engine
	status      string
	statusWin   *application.WebviewWindow
	statusTab   string // tab the status window should open on ("" = default)
	settingsWin *application.WebviewWindow
	loginWin    *application.WebviewWindow
	loginMu     sync.Mutex         // guards loginCancel
	loginCancel context.CancelFunc // cancels the in-flight login-flow poll (nil when none)
	shareWin        *application.WebviewWindow
	sharePath       string
	pendingShare    string // local path to share once the engine is ready
	versionsWin     *application.WebviewWindow
	versionPath     string
	pendingVersions string // local path to show versions for once the engine is ready
	tray            *application.SystemTray
	flyout          *application.WebviewWindow // the tray flyout panel (for live resize)
	engCancel    context.CancelFunc // stops the running engine (sign-out)
	logsWin      *application.WebviewWindow
	appWinsMu    sync.Mutex                             // guards appWins + appOpening (accessed from binding, event and main goroutines)
	appWins      map[string]*application.WebviewWindow // Nextcloud-app windows by app id
	appOpening   map[string]bool                        // opens in flight (coalesces double-clicks during the slow app-list fetch)
	pendingApp   string                                 // app id to open once the engine is ready (--app launch)
	appsCacheMu  sync.Mutex
	appsCache    []transport.App // last-fetched navigation apps (flyout refreshes keep it warm)
	appsCacheAt  time.Time
	onDemandMounts map[string]*odMount // local dir -> active mount
	pendingKeep  string // on-demand path to pin once mounts are ready
	pendingFree  string // on-demand path to free up once mounts are ready
	vfsToastMu   sync.Mutex
	lastVfsToast time.Time // rate-limits on-demand error toasts
	etags        *etagStore // last-synced server ETag per remote path (conflict baseline)
	fileids      *etagStore // remote path -> oc:fileid, for down-sync rename detection

	// Side-by-side accounts: a.eng is the PRIMARY (default) account the UI
	// shows; every other configured account runs a secondary engine that syncs
	// its own pairs concurrently in the background (toasts surface its
	// conflicts/errors; the Settings account list shows its status).
	secondaries map[string]*secondaryEngine // account ID -> background engine
	acctMu      sync.Mutex
	acctStatus  map[string]string // account ID -> latest engine status line

	licMu sync.Mutex
	lic   license.Info   // current business-licence state (gates business-tier features)
	pol   policy.Policy  // admin policy — applied ONLY when business-licensed
}

// secondaryEngine is a background sync engine for a non-primary account.
type secondaryEngine struct {
	eng    *agent.Engine
	cancel context.CancelFunc
}

// odMount is an active on-demand (virtual-files) mount.
type odMount struct {
	connKey    int64
	remoteRoot string
	watcher    *vfs.Watcher
	accountID  string // owning account (mounts are torn down with their engine)
}

// start brings up the sync engine (if an account is configured) and wires its
// status updates to frontend events. Login UI for the no-account case lands in a
// later stage.
func (a *App) start(ctx context.Context) {
	eng, err := agent.NewEngine(ctx)
	if err != nil {
		slog.Warn("engine not started", "err", err)
		a.setStatus("Not signed in")
		if a.pendingApp != "" { // a Start-menu app shortcut launched us — say why nothing opened
			notify.Toast(brand.Current.Name, "Sign in first — then "+a.pendingApp+" can open in its own window.", "")
			a.pendingApp = ""
		}
		return
	}
	a.eng = eng
	a.appsCacheMu.Lock()
	a.appsCache, a.appsCacheAt = nil, time.Time{} // stale across engine/account changes
	a.appsCacheMu.Unlock()
	// Windows whose webviews loaded before the engine existed have given up
	// polling for the theme colour by now (fresh install: the flyout loads while
	// the user is still signing in) — push the server theme to them instead.
	if a.app != nil {
		a.app.Event.Emit("accent", eng.ThemeColor())
	}
	runCtx, cancel := context.WithCancel(ctx)
	a.engCancel = cancel
	eng.SetStatusFunc(func(s string) {
		a.recordAcctStatus(eng.Account.ID, s)
		a.setStatus(s)
	})
	eng.SetAuthLostFunc(a.onAuthLost)

	// Serve per-file sync status to the Explorer overlay shell extension, and let
	// the engine poke Explorer to refresh an icon when a file's state changes.
	if _, err := overlay.Serve(a.ctx, eng.FileStatus); err != nil {
		slog.Warn("overlay status server not started", "err", err)
	}
	eng.SetOverlayRefresh(overlay.NotifyChange)

	// Live sync-progress → frontend, and animate the tray icon by state.
	eng.SetProgressFunc(func(p agent.SyncProgress) {
		if a.app != nil {
			a.app.Event.Emit("progress", toProgressDTO(p))
		}
	})
	// Desktop toasts for conflicts, can't-sync files and sync errors (subject to
	// the user's notifications preference).
	eng.SetToastFunc(notify.Toast)
	notify.SetEnabled(a.NotificationsEnabled())

	// Restore conflict policy + quiet-hours schedule from settings.
	eng.SetConflictPolicy(conflictPolicy(""))
	if d, derr := config.Resolve(); derr == nil {
		if s, serr := d.LoadSettings(); serr == nil {
			eng.SetConflictPolicy(conflictPolicy(s.ConflictPolicy))
			eng.SetPauseSchedule(agent.PauseSchedule{Enabled: s.PauseScheduleEnabled, FromMin: s.PauseFromMin, ToMin: s.PauseToMin})
		}
	}
	eng.SetPauseChangeFunc(func() { a.emit("activity"); a.rebuildTrayMenu() })
	// notify_push file changes → reconcile on-demand placeholders immediately.
	eng.SetFilesChangedFunc(a.pokeOnDemand)

	// cfapi callbacks (FETCH_DATA/FETCH_PLACEHOLDERS/…) are very chatty during
	// normal browsing, so log them at debug level — visible only with verbose on.
	cfapi.Debug = func(format string, args ...any) { slog.Debug("cfapi", "ev", fmt.Sprintf(format, args...)) }
	a.cleanupStrayOnDemand() // one-time migration: drop old experimental multi-mounts

	go a.animateTray()
	a.rebuildTrayMenu() // populate the tray right-click menu now the engine is up

	a.healBaseDir() // repair a stored baseDir that drifted from the account root

	// Keep the Explorer sidebar entry's target/icon current if it's enabled.
	if shellns.Enabled() {
		if icon, err := navIconPath(); err == nil {
			_ = shellns.Register(brand.Current.Name, a.GetBaseDir(), icon)
		}
	}

	// Forward engine change-notifications to the frontend; each goroutine exits
	// when the engine is stopped (sign-out), so they don't leak across sessions.
	forwardEvents(runCtx, eng.Recorder().Subscribe(), func() { a.emit("activity") })
	forwardEvents(runCtx, eng.Notifier().Subscribe(), func() { a.emit("notifications") })
	forwardEvents(runCtx, eng.SubscribeConflicts(), func() { a.emit("conflicts") })
	forwardEvents(runCtx, eng.SubscribeBlocked(), func() { a.emit("blocked") })

	go a.updateCheckLoop(runCtx) // periodic background "update available" toast

	var pairs []config.SyncPair
	if a.GetSyncMode() == "ondemand" {
		// On-demand mode is exclusive: the Cloud Files provider owns the files,
		// so live two-way pairs over the same tree would fight the watcher.
		a.clearLivePairs()       // files kept; leftovers can't reactivate
		a.mountAccountOnDemand() // mount the account folder (BaseDir) as virtual files
	} else {
		pairs, _ = eng.Pairs()
	}
	go func() {
		_ = eng.Run(runCtx, toAgentPairs(pairs), func(agent.Pair, transfer.Stats) {})
	}()

	// Handle a share requested before the engine was ready (e.g. the app was
	// launched directly from the Explorer "Share" menu).
	if a.pendingShare != "" {
		p := a.pendingShare
		a.pendingShare = ""
		application.InvokeAsync(func() { a.shareLocalPath(p) })
	}
	if a.pendingVersions != "" {
		p := a.pendingVersions
		a.pendingVersions = ""
		application.InvokeAsync(func() { a.versionsLocalPath(p) })
	}
	if a.pendingKeep != "" {
		p := a.pendingKeep
		a.pendingKeep = ""
		application.InvokeAsync(func() { a.keepLocalPath(p) })
	}
	if a.pendingFree != "" {
		p := a.pendingFree
		a.pendingFree = ""
		application.InvokeAsync(func() { a.freeLocalPath(p) })
	}
	if a.pendingApp != "" {
		id := a.pendingApp
		a.pendingApp = ""
		go a.openAppWindow(id) // own goroutine: it fetches the app list (network) before any window work
	}

	// Side-by-side accounts: every other configured account syncs concurrently
	// in the background.
	a.startSecondaries(ctx)
}

// recordAcctStatus stores an engine's latest status line for the per-account
// status shown in the Settings account list.
func (a *App) recordAcctStatus(accountID, s string) {
	a.acctMu.Lock()
	if a.acctStatus == nil {
		a.acctStatus = map[string]string{}
	}
	a.acctStatus[accountID] = s
	a.acctMu.Unlock()
}

// startSecondaries launches a background sync engine for every configured
// account other than the primary one. Secondaries sync their own live pairs,
// honour the same conflict policy and quiet hours, and surface problems via
// desktop toasts and the account list; the windows/flyout stay focused on the
// primary account. On-demand (virtual files) is primary-only.
func (a *App) startSecondaries(ctx context.Context) {
	if a.eng == nil {
		return
	}
	d, err := config.Resolve()
	if err != nil {
		return
	}
	st, err := account.LoadStore(d.AccountsFile())
	if err != nil {
		return
	}
	if a.secondaries == nil {
		a.secondaries = map[string]*secondaryEngine{}
	}
	primary := a.eng.Account.ID
	for _, ac := range st.Accounts {
		if ac.ID == primary {
			continue
		}
		if _, running := a.secondaries[ac.ID]; running {
			continue
		}
		eng, err := agent.NewEngineFor(ctx, ac.ID)
		if err != nil {
			slog.Warn("secondary account engine not started", "account", ac.LoginName, "err", err)
			a.recordAcctStatus(ac.ID, "Not running: "+err.Error())
			continue
		}
		runCtx, cancel := context.WithCancel(ctx)
		id := ac.ID
		eng.SetStatusFunc(func(s string) { a.recordAcctStatus(id, s); a.emit("account") })
		eng.SetToastFunc(notify.Toast)
		eng.SetAuthLostFunc(func() {
			// Only this account's sync stops — don't sign the whole app out.
			a.recordAcctStatus(id, "Sign in again")
			notify.RaiseActionable("Sign in required", eng.Account.LoginName+" couldn't authenticate — switch to that account and sign in again.", "action=login",
				[]notify.ToastButton{{Label: "Sign in", Args: "action=login"}})
			a.stopSecondary(id)
		})
		eng.SetConflictPolicy(conflictPolicy(""))
		if s, serr := d.LoadSettings(); serr == nil {
			eng.SetConflictPolicy(conflictPolicy(s.ConflictPolicy))
			eng.SetPauseSchedule(agent.PauseSchedule{Enabled: s.PauseScheduleEnabled, FromMin: s.PauseFromMin, ToMin: s.PauseToMin})
		}
		eng.SetFilesChangedFunc(a.pokeOnDemand) // push events reconcile this account's mounts too
		var pairs []config.SyncPair
		if a.GetSyncMode() == "ondemand" && cfapi.Supported() {
			// Virtual files apply to EVERY account: each gets its own virtual
			// root (its whole-account folder), so background accounts are
			// browsable on demand instead of silently live-syncing.
			a.mountSecondaryOnDemand(eng)
		} else {
			pairs, _ = eng.Pairs()
		}
		a.secondaries[id] = &secondaryEngine{eng: eng, cancel: cancel}
		slog.Info("secondary account syncing", "account", ac.LoginName, "server", ac.ServerURL, "pairs", len(pairs))
		go func() {
			_ = eng.Run(runCtx, toAgentPairs(pairs), func(agent.Pair, transfer.Stats) {})
		}()
	}
}

// mountSecondaryOnDemand mounts a background account's whole-account virtual
// root. The root is the account's own whole-account pair directory when it has
// one (e.g. from when it was the shown account); otherwise a "Nimbo - <user>"
// sibling of the primary root is created and recorded as that pair, so the
// location is stable across launches and reused by live mode later.
func (a *App) mountSecondaryOnDemand(eng *agent.Engine) {
	root := ""
	if pairs, err := eng.Pairs(); err == nil {
		for _, p := range pairs {
			if strings.Trim(p.RemoteRoot, "/") == "" {
				root = p.LocalDir
				break
			}
		}
	}
	if root == "" {
		root = filepath.Join(filepath.Dir(a.GetBaseDir()), brand.Current.Name+" - "+eng.Account.LoginName)
		if err := eng.AddSyncPair(root, ""); err != nil {
			slog.Warn("secondary on-demand: could not record account root", "account", eng.Account.LoginName, "err", err)
		}
	}
	etags, fileids := a.vfsStoresFor(eng.Account.ID)
	if err := a.mountOnDemandWith(eng, etags, fileids, root, ""); err != nil {
		slog.Warn("secondary on-demand mount failed", "account", eng.Account.LoginName, "dir", root, "err", err)
	}
}

// unmountAccount disconnects one account's on-demand mounts (used when only
// that account stops, e.g. auth loss or removal).
func (a *App) unmountAccount(accountID string) {
	for dir, m := range a.onDemandMounts {
		if m.accountID != accountID {
			continue
		}
		if m.watcher != nil {
			m.watcher.Close()
		}
		cfapi.Unmount(dir, m.connKey)
		delete(a.onDemandMounts, dir)
		slog.Info("on-demand mount disconnected", "dir", dir, "account", accountID)
	}
}

// stopSecondary stops one background account engine and its on-demand mounts.
func (a *App) stopSecondary(id string) {
	if se, ok := a.secondaries[id]; ok {
		a.unmountAccount(id)
		se.cancel()
		delete(a.secondaries, id)
	}
}

// stopSecondaries stops all background account engines.
func (a *App) stopSecondaries() {
	for id := range a.secondaries {
		a.stopSecondary(id)
	}
}

// forwardEvents runs fn whenever sub fires, until ctx is cancelled. Works for
// any channel element type (the recorder uses a typed channel).
func forwardEvents[T any](ctx context.Context, sub <-chan T, fn func()) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-sub:
				fn()
			}
		}
	}()
}

func (a *App) emit(event string) {
	if a.app != nil {
		a.app.Event.Emit(event, nil)
	}
}

func (a *App) setStatus(s string) {
	a.status = s
	if a.app != nil {
		a.app.Event.Emit("status", s)
	}
}

// --- Methods bound to the frontend ---

// Version returns the application build version.
func (a *App) Version() string { return version }

// BrandDTO carries the white-labellable identity values the frontend shows.
type BrandDTO struct {
	Name      string `json:"name"`
	Company   string `json:"company"`
	Website   string `json:"website"`
	Support   string `json:"support"`
	AccentHex string `json:"accentHex"`
}

// Brand returns this build's brand identity for the UI.
func (a *App) Brand() BrandDTO {
	return BrandDTO{
		Name: brand.Current.Name, Company: brand.Current.Company,
		Website: brand.Current.Website, Support: brand.Current.SupportEmail,
		AccentHex: brand.Current.AccentHex,
	}
}

// refreshLicence re-reads and re-validates the installed business licence into
// a.lic. Called at startup and after activation/removal.
func (a *App) refreshLicence() {
	tok := ""
	if d, err := config.Resolve(); err == nil {
		tok = license.Load(d.LicenceFile())
	}
	info := license.Evaluate(tok, time.Now())
	pol := policy.Policy{AllowSignOut: true}
	if info.Allows(license.TierBusiness) {
		pol = policy.Load() // admin policy is a business-tier capability
	}
	a.licMu.Lock()
	a.lic = info
	a.pol = pol
	a.licMu.Unlock()
}

// policyNow returns the effective admin policy (empty/permissive unless a
// business licence is active).
func (a *App) policyNow() policy.Policy {
	a.licMu.Lock()
	defer a.licMu.Unlock()
	return a.pol
}

// PolicyDTO tells the UI which settings are admin-managed so it can lock them.
type PolicyDTO struct {
	Managed       bool   `json:"managed"`
	LockServer    bool   `json:"lockServer"`
	ServerURL     string `json:"serverUrl"`
	AllowSignOut  bool   `json:"allowSignOut"`
	LockBandwidth bool   `json:"lockBandwidth"`
	LockSyncMode  bool   `json:"lockSyncMode"`
}

// PolicyInfo reports the active admin policy for the Settings UI.
func (a *App) PolicyInfo() PolicyDTO {
	p := a.policyNow()
	return PolicyDTO{
		Managed: p.Managed, LockServer: p.LockServer, ServerURL: p.ServerURL,
		AllowSignOut: p.AllowSignOut, LockBandwidth: p.LockBandwidth,
		LockSyncMode: p.SyncMode != "",
	}
}

// licence returns the current validated licence state.
func (a *App) licence() license.Info {
	a.licMu.Lock()
	defer a.licMu.Unlock()
	return a.lic
}

// licensed reports whether the given tier's features are unlocked. Business
// features call this; personal (free) features never need to.
func (a *App) licensed(t license.Tier) bool { return a.licence().Allows(t) }

// LicenceInfo reports the current licence state for the Settings UI.
func (a *App) LicenceInfo() license.Info { return a.licence() }

// ActivateLicence validates a pasted licence token and, if valid, installs it.
// Returns "" on success or a human-readable reason it was rejected.
func (a *App) ActivateLicence(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return "paste a licence key first"
	}
	if _, err := license.Verify(token); err != nil {
		return err.Error()
	}
	if license.Evaluate(token, time.Now()).Expired {
		return "that licence has expired"
	}
	d, err := config.Resolve()
	if err != nil {
		return err.Error()
	}
	if err := license.Save(d.LicenceFile(), token); err != nil {
		return err.Error()
	}
	a.refreshLicence()
	a.emit("licence")
	return ""
}

// RemoveLicence deletes the installed business licence (back to free personal).
func (a *App) RemoveLicence() string {
	if d, err := config.Resolve(); err == nil {
		if err := license.Remove(d.LicenceFile()); err != nil {
			return err.Error()
		}
	}
	a.refreshLicence()
	a.emit("licence")
	return ""
}

// UpdateDTO reports the result of an update check for the Settings UI.
type UpdateDTO struct {
	Current   string `json:"current"`
	Latest    string `json:"latest"`
	Available bool   `json:"available"`
	URL       string `json:"url"`
	Notes     string `json:"notes"` // release notes ("what's in this update")
	Err       string `json:"err"`
}

// CheckForUpdate queries the GitHub releases for a newer build.
func (a *App) CheckForUpdate() UpdateDTO {
	rel, avail, err := update.Check(a.ctx, version)
	dto := UpdateDTO{Current: version, Latest: rel.Tag, Available: avail, URL: rel.URL, Notes: strings.TrimSpace(rel.Body)}
	if err != nil {
		dto.Err = err.Error()
	}
	return dto
}

// updateCheckLoop periodically checks for a newer release and toasts when one is
// found. Nimbo is a long-running tray app, so the App Installer feed's OnLaunch
// check rarely fires for it; this keeps users aware. It only notifies — the user
// applies it from Settings ("Update now") — and only runs on packaged installs.
// It notifies once per version per session to avoid nagging.
func (a *App) updateCheckLoop(ctx context.Context) {
	if !canApplyUpdate() {
		return // loose dev build: nothing to self-update
	}
	timer := time.NewTimer(2 * time.Minute) // first check shortly after startup
	defer timer.Stop()
	var lastNotified string
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if rel, avail, err := update.Check(ctx, version); err == nil && avail && rel.Tag != lastNotified {
			lastNotified = rel.Tag
			notify.RaiseActionable(brand.Current.Name+" update available",
				rel.Tag+" is ready to install.", "action=settings",
				[]notify.ToastButton{{Label: "Update now", Args: "action=update"}})
		}
		timer.Reset(24 * time.Hour)
	}
}

// CanApplyUpdate reports whether Nimbo can update itself in place (a packaged
// install on Windows). The frontend offers "Update now" only when true; loose
// dev builds fall back to opening the release page.
func (a *App) CanApplyUpdate() bool { return canApplyUpdate() }

// ApplyUpdate installs the latest release and relaunches. Nimbo closes partway
// through (the package can't update while running). Returns "" once the update
// helper has started, or an error message.
//
// It installs the release's own versioned .msix asset URL (from the GitHub API)
// rather than going through the latest/download App Installer feed: both
// GitHub's CDN alias and Windows' cached copy of the .appinstaller can lag a
// release, making Add-AppxPackage conclude "already up to date" and "succeed"
// without changing the installed version. A per-version asset URL can't be
// stale.
func (a *App) ApplyUpdate() string {
	if !canApplyUpdate() {
		return "in-app update is only available in the installed app"
	}
	rel, avail, err := update.Check(a.ctx, version)
	if err != nil {
		return "couldn't fetch the latest release: " + err.Error()
	}
	if !avail {
		return "no newer version is available"
	}
	msix := rel.Asset(".msix")
	if msix.DownloadURL == "" {
		return "latest release (" + rel.Tag + ") has no .msix asset"
	}
	if err := applyUpdate(msix.DownloadURL); err != nil {
		return err.Error()
	}
	// Quit ourselves rather than leaving it to the installer. Nothing outside the
	// process can stop a tray app: WM_CLOSE — all an external helper can send —
	// closes a window, not the app, so the helper's graceful phase always ran out
	// its deadline and force-killed us (10.7s wasted installing 0.1.0.139), which
	// also skipped unmountAllOnDemand. Quitting here unwinds app.Run() normally so
	// the on-demand mounts disconnect cleanly, and the helper then finds nothing
	// left to close. The brief delay lets this return first, so the UI can show
	// "Updating…" before the window goes.
	go func() {
		time.Sleep(1500 * time.Millisecond)
		a.Quit()
	}()
	return ""
}

// ThemeColor returns the user's Nextcloud primary theme colour (hex) to accent
// the UI, or "" to use the default.
func (a *App) ThemeColor() string {
	if a.eng == nil {
		return ""
	}
	return a.eng.ThemeColor()
}

// OnDemandSupported reports whether on-demand files can be configured here.
func (a *App) OnDemandSupported() bool { return cfapi.Supported() }

// GetSyncMode returns "live" (default) or "ondemand".
func (a *App) GetSyncMode() string {
	if pm := a.policyNow().SyncMode; pm != "" {
		return pm // admin-enforced file-availability mode
	}
	if d, err := config.Resolve(); err == nil {
		if s, e := d.LoadSettings(); e == nil && s.SyncMode != "" {
			return s.SyncMode
		}
	}
	return "live"
}

// SetSyncMode switches the account between "live" and "ondemand". On-demand
// clears any live sync pairs (the Cloud Files provider owns the files; an
// overlapping pair would fight the placeholder watcher) and mounts the account
// folder. Switching back to live unmounts it. Local files are left in place.
func (a *App) SetSyncMode(mode string) string {
	if a.policyNow().SyncMode != "" {
		return "File availability is managed by your organisation."
	}
	if d, err := config.Resolve(); err == nil {
		if s, e := d.LoadSettings(); e == nil {
			s.SyncMode = mode
			_ = d.SaveSettings(s)
		}
	}
	// The mode shapes every account's engine setup (live pairs vs virtual
	// roots), so restart the stack under the new mode: start() mounts the
	// primary and startSecondaries() handles the background accounts.
	a.unmountAllOnDemand()
	a.stopEngine()
	a.start(a.ctx)
	if a.eng == nil {
		return "couldn't restart syncing — check the logs"
	}
	return ""
}

// clearLivePairs removes every configured live sync pair (config + its watcher)
// without deleting local files. Used when entering on-demand mode.
func (a *App) clearLivePairs() {
	if a.eng == nil {
		return
	}
	pairs, _ := a.eng.Pairs()
	for _, p := range pairs {
		if err := a.eng.RemoveSyncFolder(p.RemoteRoot, false); err != nil {
			slog.Warn("clear live pair", "remote", p.RemoteRoot, "err", err)
			continue
		}
		slog.Info("cleared live sync pair for on-demand mode", "remote", p.RemoteRoot, "local", p.LocalDir)
	}
	a.rebuildTrayMenu()
}

// --- On-demand (virtual files): one account-level mount ---
//
// On-demand is a single account mode (Settings.SyncMode == "ondemand"): the
// account's local folder (BaseDir) is mounted as one whole-account cloud sync
// root. It's set at account setup and reconnected automatically every launch —
// there is no per-folder mount tool.

// mountAccountOnDemand mounts the account folder (BaseDir) as the whole-account
// virtual-files sync root. Idempotent-ish: call after clearing live pairs.
func (a *App) mountAccountOnDemand() {
	if a.eng == nil || !cfapi.Supported() {
		return
	}
	dir := a.GetBaseDir()
	if _, already := a.onDemandMounts[dir]; already {
		return
	}
	if err := a.mountOnDemand(dir, ""); err != nil {
		slog.Warn("on-demand account mount failed", "dir", dir, "err", err)
	}
}

// vfsStoresFor opens (or reuses) an account's persistent on-demand stores: the
// remote-path -> ETag conflict baselines and the remote-path -> oc:fileid map
// for down-sync rename detection. Per ACCOUNT — remote paths from two servers
// must never mix.
func (a *App) vfsStoresFor(accountID string) (etags, fileids *etagStore) {
	ep, fp := "vfs-etags.json", "vfs-fileids.json"
	if d, derr := config.Resolve(); derr == nil {
		d = d.WithAccount(accountID)
		d.MigratePairs()
		ep, fp = d.VFSETagsFile(), d.VFSFileIDsFile()
	}
	return newEtagStore(ep), newEtagStore(fp)
}

// mountOnDemand connects localDir as a cloud sync root for the PRIMARY account.
func (a *App) mountOnDemand(localDir, remoteRoot string) error {
	if a.eng == nil {
		return fmt.Errorf("not signed in")
	}
	if a.etags == nil || a.fileids == nil {
		a.etags, a.fileids = a.vfsStoresFor(a.eng.Account.ID)
	}
	return a.mountOnDemandWith(a.eng, a.etags, a.fileids, localDir, remoteRoot)
}

// mountOnDemandWith connects localDir as a cloud sync root mirroring remoteRoot
// on the given account's engine, and starts the write-back watcher. Each
// account's mounts use that account's own etag/fileid stores.
func (a *App) mountOnDemandWith(eng *agent.Engine, etags, fileids *etagStore, localDir, remoteRoot string) error {
	if !cfapi.Supported() {
		return fmt.Errorf("on-demand files aren't supported on this system")
	}
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		return err
	}
	root := strings.Trim(remoteRoot, "/")
	// report records a VFS operation in the activity feed (which auto-refreshes
	// the flyout via the recorder subscription) and toasts on error.
	report := func(kind, remotePath string, err error) {
		ev := activity.Event{Local: localDir, Path: remotePath, Kind: kind}
		if err != nil {
			ev.Err = err.Error()
		}
		eng.Recorder().Add(ev)
		if err != nil {
			a.vfsErrorToast(kind, remotePath, err)
		}
	}
	hydrate := func(identity []byte, offset, length int64) ([]byte, error) {
		data, err := eng.DownloadRange(a.ctx, string(identity), offset, length)
		if offset == 0 { // record once per file-open, not per chunk
			report("download", string(identity), err)
		}
		return data, err
	}
	// listRemote returns a directory's children as placeholders (rel is
	// sync-root-relative, "" = root). Each entry's identity is its remote path so
	// hydrate can fetch it later. Used both for on-demand population and for
	// down-sync reconciliation, which needs the error to distinguish a failed
	// listing from an empty directory.
	listRemote := func(rel string) ([]cfapi.PlaceholderInfo, error) {
		remote := strings.Trim(root+"/"+rel, "/")
		entries, err := eng.Browse(a.ctx, remote)
		if err != nil {
			return nil, err
		}
		var items []cfapi.PlaceholderInfo
		for _, e := range entries {
			p := strings.Trim(e.Path, "/")
			if p == remote || p == "" {
				continue // the directory itself
			}
			name := p
			if i := strings.LastIndex(p, "/"); i >= 0 {
				name = p[i+1:]
			}
			if e.IsDir && e.IsEncrypted {
				// End-to-end encrypted folder: its contents are opaque ciphertext
				// without the E2EE keys, so don't surface it as placeholders.
				slog.Info("on-demand: skipping end-to-end encrypted folder", "path", p)
				continue
			}
			items = append(items, cfapi.PlaceholderInfo{
				Name: name, Size: e.Size, IsDir: e.IsDir, ModTime: e.LastModified,
				Identity: []byte(p), ETag: e.ETag, FileID: e.FileID,
			})
		}
		return items, nil
	}
	// Population is best-effort (an error just shows the folder empty until retry).
	// It also records the conflict baseline (server ETag) for each placeholder.
	list := func(rel string) []cfapi.PlaceholderInfo {
		items, err := listRemote(rel)
		if err != nil {
			slog.Warn("on-demand list failed", "rel", rel, "err", err)
		}
		base := make(map[string]string, len(items))
		fids := make(map[string]string, len(items))
		for _, it := range items {
			if !it.IsDir {
				base[string(it.Identity)] = it.ETag
				if it.FileID != "" {
					fids[string(it.Identity)] = it.FileID
				}
			}
		}
		etags.setMany(base)
		fileids.setMany(fids)
		return items
	}
	exe, _ := os.Executable()
	connKey, err := cfapi.Mount(localDir, brand.Current.Name, exe, hydrate, list)
	if err != nil {
		return err
	}
	// Write-back + down-sync: watch the mount for user changes (upload/mkdir/
	// delete/move) and pull changes made elsewhere (List). When notify_push is
	// available the reconcile is driven by push (Poke), so the poll is a long
	// safety net; otherwise it's the primary trigger.
	poll := 30 * time.Second
	if eng.PushAvailable() {
		poll = 5 * time.Minute
	}
	w, werr := vfs.New(a.ctx, localDir, root, poll, vfs.Ops{
		Upload:         a.uploadWithConflictFor(eng, etags),
		Mkdir:          eng.MkdirRemote,
		Delete:         eng.DeleteRemote,
		Move:           eng.MoveRemote,
		List:           listRemote,
		Report:         report,
		RecordBaseline: func(remote, etag string) { etags.set(remote, etag) },
		Baseline:       func(remote string) (string, bool) { e := etags.get(remote); return e, e != "" },
		RecordFileID:   func(remote, fid string) { fileids.set(remote, fid) },
		FileID:         func(remote string) (string, bool) { f := fileids.get(remote); return f, f != "" },
		DropFileID:     func(remote string) { fileids.del(remote) },
		Log:            func(f string, args ...any) { slog.Info("vfs", "msg", fmt.Sprintf(f, args...)) },
	})
	if werr != nil {
		slog.Warn("vfs write-back watcher not started", "dir", localDir, "err", werr)
	}
	if a.onDemandMounts == nil {
		a.onDemandMounts = map[string]*odMount{}
	}
	a.onDemandMounts[localDir] = &odMount{connKey: connKey, remoteRoot: root, watcher: w, accountID: eng.Account.ID}
	slog.Info("on-demand mount connected", "dir", localDir, "remoteRoot", root)
	return nil
}

// vfsErrorToast shows a desktop toast for a failed on-demand operation,
// rate-limited so a burst of failures (e.g. quota exceeded) doesn't spam.
func (a *App) vfsErrorToast(kind, remotePath string, err error) {
	if !a.NotificationsEnabled() {
		return
	}
	a.vfsToastMu.Lock()
	if time.Since(a.lastVfsToast) < 8*time.Second {
		a.vfsToastMu.Unlock()
		return
	}
	a.lastVfsToast = time.Now()
	a.vfsToastMu.Unlock()

	verb := map[string]string{
		"upload": "upload", "download": "download", "delete-remote": "delete",
		"move": "move", "mkdir-remote": "create folder",
	}[kind]
	if verb == "" {
		verb = kind
	}
	msg := fmt.Sprintf("Couldn't %s %s", verb, filepath.Base(remotePath))
	if strings.Contains(err.Error(), "507") || strings.Contains(strings.ToLower(err.Error()), "quota") {
		msg += " — server storage is full"
	}
	notify.Toast(brand.Current.Name+" — on-demand sync", msg, "")
}

// uploadWithConflictFor builds the on-demand upload op for one account's
// engine: if the server copy changed since we last synced it (its ETag differs
// from the recorded baseline), both sides were edited — the server's version is
// moved aside to a "conflicted copy" so it isn't lost, then ours is uploaded
// and the new baseline recorded.
func (a *App) uploadWithConflictFor(eng *agent.Engine, etags *etagStore) func(ctx context.Context, localPath, remotePath string) error {
	return func(ctx context.Context, localPath, remotePath string) error {
		remotePath = strings.Trim(remotePath, "/")
		base := etags.get(remotePath)
		if base != "" {
			if cur, ok, err := eng.StatRemote(ctx, remotePath); err == nil && ok && cur.ETag != base {
				// Server changed too — keep both: park the server's version.
				conflict := conflictName(remotePath)
				if mErr := eng.MoveRemote(ctx, remotePath, conflict); mErr != nil {
					slog.Warn("vfs conflict: could not park server copy", "remote", remotePath, "err", mErr)
				} else {
					slog.Info("vfs conflict: kept both", "remote", remotePath, "serverCopy", conflict)
					eng.Recorder().Add(activity.Event{Path: conflict, Kind: "conflict"})
					notify.Toast("Sync conflict", filepath.Base(remotePath)+" was edited in both places — kept both copies", "")
				}
			}
		}
		if err := eng.Upload(ctx, localPath, remotePath); err != nil {
			return err
		}
		if cur, ok, err := eng.StatRemote(ctx, remotePath); err == nil && ok {
			etags.set(remotePath, cur.ETag) // baseline now matches what we uploaded
		}
		return nil
	}
}

// conflictName builds a "<name> (conflicted copy <date>)<ext>" sibling path.
func conflictName(remote string) string {
	dir, file := "", strings.Trim(remote, "/")
	if i := strings.LastIndex(file, "/"); i >= 0 {
		dir, file = file[:i+1], file[i+1:]
	}
	stem, ext := file, ""
	if j := strings.LastIndex(file, "."); j > 0 {
		stem, ext = file[:j], file[j:]
	}
	return dir + stem + " (conflicted copy " + time.Now().Format("2006-01-02 150405") + ")" + ext
}

// OfflineEntry is one folder in the "available offline" browser.
type OfflineEntry struct {
	Name   string `json:"name"`
	Rel    string `json:"rel"`
	Pinned bool   `json:"pinned"`
}

// BrowseOffline lists the shown account's virtual-root folders at rel ("" =
// root) with their pin state, for the available-offline panel. Only folders
// that already exist locally as placeholders appear (the tree populates as
// it's browsed).
func (a *App) BrowseOffline(rel string) []OfflineEntry {
	if a.eng == nil || !cfapi.Supported() {
		return nil
	}
	dir := a.GetBaseDir()
	if rel != "" {
		dir = filepath.Join(dir, filepath.FromSlash(rel))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := []OfflineEntry{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		r := e.Name()
		if rel != "" {
			r = rel + "/" + e.Name()
		}
		out = append(out, OfflineEntry{
			Name:   e.Name(),
			Rel:    r,
			Pinned: cfapi.PinStateOf(filepath.Join(dir, e.Name())) == "pinned",
		})
	}
	return out
}

// SetOfflinePin pins (always keep on this device — fully downloads and stays
// current) or unpins (back to online-only preference) a folder subtree in the
// shown account's virtual root.
func (a *App) SetOfflinePin(rel string, pinned bool) string {
	if a.eng == nil {
		return "not signed in"
	}
	p := filepath.Join(a.GetBaseDir(), filepath.FromSlash(rel))
	if err := cfapi.SetPinState(p, pinned, true); err != nil {
		return err.Error()
	}
	return ""
}

// pokeOnDemand asks every active on-demand mount to reconcile (push-driven).
func (a *App) pokeOnDemand() {
	for _, m := range a.onDemandMounts {
		if m.watcher != nil {
			m.watcher.Poke()
		}
	}
}

// unmountAllOnDemand stops the active mount (graceful shutdown / leaving
// on-demand mode). Placeholders stay on disk and are reconnected next launch.
func (a *App) unmountAllOnDemand() {
	for dir, m := range a.onDemandMounts {
		if m.watcher != nil {
			m.watcher.Close()
		}
		cfapi.Unmount(dir, m.connKey)
		delete(a.onDemandMounts, dir)
	}
}

// cleanupStrayOnDemand removes the sync roots left by the old experimental
// multi-mount model (a one-time migration). It purges each stray folder, not
// just its registration: an unregistered-but-on-disk placeholder tree is
// stranded (the cldflt filter reports "the cloud file metadata is corrupt and
// unreadable" on every access, blocking even deletion), so the files must be
// removed while a provider is connected. Their content lives on the server, so
// this is non-destructive. The account folder is re-mounted fresh afterwards by
// mountAccountOnDemand.
func (a *App) cleanupStrayOnDemand() {
	if !cfapi.Supported() {
		return
	}
	d, err := config.Resolve()
	if err != nil {
		return
	}
	s, err := d.LoadSettings()
	if err != nil || len(s.OnDemandMounts) == 0 {
		return
	}
	cfapi.UnregisterLegacyShellSyncRoot() // drop the old fixed-id shell entry
	for _, m := range s.OnDemandMounts {
		if err := cfapi.Purge(m.Local); err != nil {
			slog.Warn("purge stray on-demand folder", "dir", m.Local, "err", err)
		} else {
			slog.Info("purged stray on-demand folder", "dir", m.Local)
		}
	}
	s.OnDemandMounts = nil
	_ = d.SaveSettings(s)
}

// NextcloudAppearance reports the user's Nextcloud appearance: "dark", "light",
// or "default" (they follow the system). Returns "" if it can't be determined
// (engine not ready / network error). Used by the "Match Nextcloud theme" mode.
func (a *App) NextcloudAppearance() string {
	if a.eng == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(a.ctx, 8*time.Second)
	defer cancel()
	s, err := a.eng.ThemeAppearance(ctx)
	if err != nil {
		slog.Debug("nextcloud appearance fetch failed", "err", err)
		return ""
	}
	return s
}

// GetTheme returns the UI theme mode: "system" (default), "light", or "dark".
func (a *App) GetTheme() string {
	if d, err := config.Resolve(); err == nil {
		if s, e := d.LoadSettings(); e == nil && s.Theme != "" {
			return s.Theme
		}
	}
	return "system"
}

// SetTheme persists the UI theme mode and broadcasts it so every window updates.
func (a *App) SetTheme(mode string) {
	if d, err := config.Resolve(); err == nil {
		if s, e := d.LoadSettings(); e == nil {
			s.Theme = mode
			_ = d.SaveSettings(s)
		}
	}
	if a.app != nil {
		a.app.Event.Emit("theme", mode)
	}
}

// FlyoutAppearanceDTO carries the flyout's appearance settings to the UI.
type FlyoutAppearanceDTO struct {
	DockIconSize string   `json:"dockIconSize"` // small | medium | large
	PanelWidth   string   `json:"panelWidth"`   // compact | standard | wide
	Density      string   `json:"density"`      // comfortable | compact
	Sections     []string `json:"sections"`     // ordered visible middle sections
}

func defaultFlyoutSections() []string { return []string{"search", "activity", "storage"} }

func sliceHas(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}

// flyoutWidthFor maps a panel-width setting to a pixel width.
func flyoutWidthFor(pw string) int {
	switch pw {
	case "compact":
		return 320
	case "wide":
		return 440
	default:
		return 360
	}
}

// FlyoutAppearance returns the current flyout appearance, with defaults filled in.
func (a *App) FlyoutAppearance() FlyoutAppearanceDTO {
	dto := FlyoutAppearanceDTO{DockIconSize: "medium", PanelWidth: "standard", Density: "comfortable", Sections: defaultFlyoutSections()}
	if d, err := config.Resolve(); err == nil {
		if s, e := d.LoadSettings(); e == nil {
			if s.DockIconSize != "" {
				dto.DockIconSize = s.DockIconSize
			}
			if s.PanelWidth != "" {
				dto.PanelWidth = s.PanelWidth
			}
			if s.Density != "" {
				dto.Density = s.Density
			}
			if len(s.FlyoutSections) > 0 {
				dto.Sections = s.FlyoutSections
			} else if s.HideSearch {
				dto.Sections = []string{"activity", "storage"} // migrate the legacy hide-search flag
			}
		}
	}
	return dto
}

// SetFlyoutAppearance persists the flyout appearance, resizes the panel, and
// broadcasts so an open flyout updates live.
func (a *App) SetFlyoutAppearance(dto FlyoutAppearanceDTO) {
	if d, err := config.Resolve(); err == nil {
		if s, e := d.LoadSettings(); e == nil {
			s.DockIconSize, s.PanelWidth, s.Density = dto.DockIconSize, dto.PanelWidth, dto.Density
			s.FlyoutSections = dto.Sections
			s.HideSearch = !sliceHas(dto.Sections, "search") // keep the legacy flag in step
			_ = d.SaveSettings(s)
		}
	}
	a.applyFlyoutWidth(dto.PanelWidth)
	a.emit("appearance")
}

// applyFlyoutWidth resizes the tray flyout window to match the panel-width setting.
func (a *App) applyFlyoutWidth(pw string) {
	if a.flyout != nil {
		a.flyout.SetSize(flyoutWidthFor(pw), flyoutHeight)
	}
}

// ShowAppDock reports whether the flyout's right-side app dock is shown
// (default true).
func (a *App) ShowAppDock() bool {
	if d, err := config.Resolve(); err == nil {
		if s, e := d.LoadSettings(); e == nil {
			return !s.HideAppDock
		}
	}
	return true
}

// SetShowAppDock persists whether the app dock is shown and broadcasts it so the
// flyout updates live.
func (a *App) SetShowAppDock(on bool) {
	if d, err := config.Resolve(); err == nil {
		if s, e := d.LoadSettings(); e == nil {
			s.HideAppDock = !on
			_ = d.SaveSettings(s)
		}
	}
	if a.app != nil {
		a.app.Event.Emit("appdock", on)
	}
}

// AppDockSide reports which edge the app dock sits on: "right" (default),
// "left", or "bottom".
func (a *App) AppDockSide() string {
	if d, err := config.Resolve(); err == nil {
		if s, e := d.LoadSettings(); e == nil && (s.AppDockSide == "left" || s.AppDockSide == "bottom") {
			return s.AppDockSide
		}
	}
	return "right"
}

// SetAppDockSide persists the dock edge ("left", "right", or "bottom") and
// broadcasts it so the flyout moves the rail live.
func (a *App) SetAppDockSide(side string) {
	if side != "left" && side != "bottom" {
		side = "right"
	}
	if d, err := config.Resolve(); err == nil {
		if s, e := d.LoadSettings(); e == nil {
			s.AppDockSide = side
			_ = d.SaveSettings(s)
		}
	}
	if a.app != nil {
		a.app.Event.Emit("appdock-side", side)
	}
}

// ShowSearch reports whether the flyout's file-search bar is shown (default
// true).
func (a *App) ShowSearch() bool {
	if d, err := config.Resolve(); err == nil {
		if s, e := d.LoadSettings(); e == nil {
			return !s.HideSearch
		}
	}
	return true
}

// SetShowSearch persists whether the search bar is shown and broadcasts it so
// the flyout updates live.
func (a *App) SetShowSearch(on bool) {
	if d, err := config.Resolve(); err == nil {
		if s, e := d.LoadSettings(); e == nil {
			s.HideSearch = !on
			_ = d.SaveSettings(s)
		}
	}
	if a.app != nil {
		a.app.Event.Emit("searchbar", on)
	}
}

// LowMemoryMode reports whether the baseline is read from disk per sync (small
// footprint) rather than held resident in RAM (faster warm syncs). Default true.
func (a *App) LowMemoryMode() bool {
	if d, err := config.Resolve(); err == nil {
		if s, e := d.LoadSettings(); e == nil {
			return !s.KeepBaselineInMemory
		}
	}
	return true
}

// SetLowMemoryMode toggles the resident-baseline cache and reopens the store so it
// takes effect immediately (freeing the cache when turning low-memory on).
func (a *App) SetLowMemoryMode(on bool) {
	if d, err := config.Resolve(); err == nil {
		if s, e := d.LoadSettings(); e == nil {
			s.KeepBaselineInMemory = !on
			_ = d.SaveSettings(s)
		}
	}
	if a.eng != nil {
		a.eng.ReloadStore()
	}
}

// NotificationsEnabled reports whether desktop toasts are turned on.
func (a *App) NotificationsEnabled() bool {
	if d, err := config.Resolve(); err == nil {
		if s, e := d.LoadSettings(); e == nil {
			return !s.MuteNotifications
		}
	}
	return true
}

// SetNotifications turns desktop toasts on or off and persists the choice.
func (a *App) SetNotifications(on bool) {
	notify.SetEnabled(on)
	if d, err := config.Resolve(); err == nil {
		if s, e := d.LoadSettings(); e == nil {
			s.MuteNotifications = !on
			_ = d.SaveSettings(s)
		}
	}
}

// Status returns the current sync status text.
func (a *App) Status() string {
	if a.status == "" {
		return "Starting…"
	}
	return a.status
}

// ProgressDTO is a live sync-progress snapshot for the flyout.
type ProgressDTO struct {
	Active      bool   `json:"active"`
	Current     string `json:"current"`
	Done        int    `json:"done"`
	Total       int    `json:"total"`
	Speed       int64  `json:"speed"`
	AvgSpeed    int64  `json:"avgSpeed"`
	DoneBytes   int64  `json:"doneBytes"`
	TotalBytes  int64  `json:"totalBytes"`
	Enumerating bool   `json:"enumerating"`
}

func toProgressDTO(p agent.SyncProgress) ProgressDTO {
	return ProgressDTO{
		Active:      p.Active,
		Current:     p.Current,
		Done:        p.Done,
		Total:       p.Total,
		Speed:       p.Speed,
		AvgSpeed:    p.AvgSpeed,
		DoneBytes:   p.DoneBytes,
		TotalBytes:  p.TotalBytes,
		Enumerating: p.Enumerating,
	}
}

// Progress returns the current sync-progress snapshot.
func (a *App) Progress() ProgressDTO {
	if a.eng == nil {
		return ProgressDTO{}
	}
	return toProgressDTO(a.eng.Progress())
}

// animateTray updates the tray icon to reflect sync state, spinning it while
// syncing. It runs until the app context is cancelled.
func (a *App) animateTray() {
	if a.tray == nil {
		return
	}
	t := time.NewTicker(120 * time.Millisecond)
	defer t.Stop()
	frame, last := 0, ""
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-t.C:
			state := a.trayState()
			badge := a.trayBadge()
			key := state
			if badge {
				key += "+badge"
			}
			if state == "sync" {
				frame++
				a.tray.SetIcon(trayIcon("sync", frame, badge))
				last = key
			} else if key != last {
				a.tray.SetIcon(trayIcon(state, 0, badge))
				last = key
			}
		}
	}
}

func (a *App) trayState() string {
	if a.eng != nil && a.eng.Paused() {
		return "paused"
	}
	s := strings.ToLower(a.status)
	switch {
	case strings.Contains(s, "sync"):
		return "sync"
	case strings.Contains(s, "error"):
		return "error"
	default:
		return "idle"
	}
}

// trayBadge reports whether there are pending Nextcloud notifications.
func (a *App) trayBadge() bool {
	return a.eng != nil && a.eng.Notifier().Count() > 0
}

// NotificationCount returns the number of pending Nextcloud notifications, for
// the flyout's notification-bell badge.
func (a *App) NotificationCount() int {
	if a.eng == nil {
		return 0
	}
	return a.eng.Notifier().Count()
}

// HeaderInfo describes the signed-in user and their Nextcloud presence.
type HeaderInfo struct {
	User       string `json:"user"`
	Server     string `json:"server"`
	StatusType string `json:"statusType"` // online | away | dnd | invisible | offline
	StatusMsg  string `json:"statusMsg"`
	StatusIcon string `json:"statusIcon"`

	QuotaUsed  int64   `json:"quotaUsed"`  // bytes used
	QuotaTotal int64   `json:"quotaTotal"` // effective total bytes (0 if unknown)
	QuotaPct   float64 `json:"quotaPct"`   // percent used (0–100)
	Unlimited  bool    `json:"unlimited"`  // no configured quota
}

// Header returns the account name, Nextcloud presence and storage quota for the
// flyout header.
func (a *App) Header() HeaderInfo {
	if a.eng == nil {
		return HeaderInfo{}
	}
	h := HeaderInfo{User: a.eng.Account.LoginName, Server: a.eng.Account.ServerURL}
	if us, err := a.eng.UserStatus(a.ctx); err == nil {
		h.StatusType, h.StatusMsg, h.StatusIcon = us.Status, us.Message, us.Icon
	}
	if q, err := a.eng.Quota(a.ctx); err == nil {
		h.QuotaUsed, h.QuotaTotal, h.QuotaPct = q.Used, q.Total, q.Relative
		h.Unlimited = q.Limit < 0
	}
	return h
}

// AttentionInfo counts items needing the user's attention.
type AttentionInfo struct {
	Conflicts int `json:"conflicts"`
	Blocked   int `json:"blocked"`
}

// Attention reports how many conflicts and can't-sync files are outstanding.
func (a *App) Attention() AttentionInfo {
	if a.eng == nil {
		return AttentionInfo{}
	}
	info := AttentionInfo{
		Conflicts: len(a.eng.PendingConflicts()),
		Blocked:   len(a.eng.BlockedFiles()),
	}
	// Background accounts' conflicts/blocked count toward the badge so they
	// can't go unnoticed; their lists live behind a "Show" switch (the resolve
	// actions operate on the shown account).
	for _, se := range a.secondaries {
		info.Conflicts += len(se.eng.PendingConflicts())
		info.Blocked += len(se.eng.BlockedFiles())
	}
	return info
}

// OtherAttention reports a background account with pending conflicts or
// blocked files, so the Status window can point at it.
type OtherAttention struct {
	ID        string `json:"id"`
	User      string `json:"user"`
	Conflicts int    `json:"conflicts"`
	Blocked   int    `json:"blocked"`
}

// OtherAccountAttention lists background accounts that need attention.
func (a *App) OtherAccountAttention() []OtherAttention {
	out := []OtherAttention{}
	for id, se := range a.secondaries {
		c, b := len(se.eng.PendingConflicts()), len(se.eng.BlockedFiles())
		if c+b > 0 {
			out = append(out, OtherAttention{ID: id, User: se.eng.Account.LoginName, Conflicts: c, Blocked: b})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].User < out[j].User })
	return out
}

// SetStatusType sets the user's Nextcloud presence (online/away/dnd/invisible).
func (a *App) SetStatusType(t string) {
	if a.eng != nil {
		_ = a.eng.SetUserStatusType(a.ctx, t)
	}
}

// SetStatusMessage sets a custom status message.
func (a *App) SetStatusMessage(msg string) {
	if a.eng != nil {
		_ = a.eng.SetUserStatusMessage(a.ctx, msg, "")
	}
}

// ClearStatusMessage clears the custom status message.
func (a *App) ClearStatusMessage() {
	if a.eng != nil {
		_ = a.eng.ClearUserStatusMessage(a.ctx)
	}
}

// SyncNow triggers an immediate sync of all folders.
func (a *App) SyncNow() {
	if a.eng != nil {
		a.eng.TriggerSync()
	}
}

// Paused reports whether syncing is paused.
func (a *App) Paused() bool { return a.eng != nil && a.eng.Paused() }

// TogglePause flips the paused state and returns the new value.
func (a *App) TogglePause() bool {
	if a.eng == nil {
		return false
	}
	p := !a.eng.Paused()
	a.eng.SetPaused(p)
	a.rebuildTrayMenu() // update the Pause/Resume label
	return p
}

// PauseFor pauses syncing for the given minutes (0 = indefinitely).
func (a *App) PauseFor(minutes int) {
	if a.eng == nil {
		return
	}
	if minutes <= 0 {
		a.eng.SetPaused(true)
	} else {
		a.eng.PauseFor(time.Duration(minutes) * time.Minute)
	}
	a.rebuildTrayMenu()
}

// PauseUntilTomorrow pauses until 08:00 the next day.
func (a *App) PauseUntilTomorrow() {
	if a.eng == nil {
		return
	}
	now := time.Now()
	t := time.Date(now.Year(), now.Month(), now.Day(), 8, 0, 0, 0, now.Location())
	if !t.After(now) {
		t = t.Add(24 * time.Hour)
	}
	a.eng.PauseFor(t.Sub(now))
	a.rebuildTrayMenu()
}

// Resume clears any pause (manual or timed).
func (a *App) Resume() {
	if a.eng != nil {
		a.eng.SetPaused(false)
		a.rebuildTrayMenu()
	}
}

// PauseDTO mirrors the engine's effective pause state for the UI.
type PauseDTO struct {
	Paused bool   `json:"paused"`
	Reason string `json:"reason"`
	Until  string `json:"until"`
}

// PauseInfo returns the current pause state.
func (a *App) PauseInfo() PauseDTO {
	if a.eng == nil {
		return PauseDTO{}
	}
	s := a.eng.PauseState()
	return PauseDTO{Paused: s.Paused, Reason: s.Reason, Until: s.Until}
}

// ScheduleDTO is the quiet-hours auto-pause configuration.
type ScheduleDTO struct {
	Enabled bool `json:"enabled"`
	FromMin int  `json:"fromMin"`
	ToMin   int  `json:"toMin"`
}

// GetPauseSchedule returns the saved quiet-hours window.
func (a *App) GetPauseSchedule() ScheduleDTO {
	if d, err := config.Resolve(); err == nil {
		if s, e := d.LoadSettings(); e == nil {
			return ScheduleDTO{Enabled: s.PauseScheduleEnabled, FromMin: s.PauseFromMin, ToMin: s.PauseToMin}
		}
	}
	return ScheduleDTO{}
}

// SetPauseSchedule saves and applies the quiet-hours window.
func (a *App) SetPauseSchedule(enabled bool, fromMin, toMin int) {
	if d, err := config.Resolve(); err == nil {
		if s, e := d.LoadSettings(); e == nil {
			s.PauseScheduleEnabled = enabled
			s.PauseFromMin = fromMin
			s.PauseToMin = toMin
			_ = d.SaveSettings(s)
		}
	}
	if a.eng != nil {
		a.eng.SetPauseSchedule(agent.PauseSchedule{Enabled: enabled, FromMin: fromMin, ToMin: toMin})
	}
	a.rebuildTrayMenu()
}

// FolderInfo is a synced folder for the flyout's quick buttons.
type FolderInfo struct {
	Name     string `json:"name"`
	LocalDir string `json:"localDir"`
}

// Folders returns the quick-open folders for the flyout: live sync pairs plus
// any on-demand (virtual files) mounts.
func (a *App) Folders() []FolderInfo {
	if a.eng == nil {
		return nil
	}
	out := []FolderInfo{}
	pairs, _ := a.eng.Pairs()
	for _, p := range pairs {
		out = append(out, FolderInfo{Name: filepath.Base(p.LocalDir), LocalDir: p.LocalDir})
	}
	mounts := make([]string, 0, len(a.onDemandMounts))
	for dir := range a.onDemandMounts {
		mounts = append(mounts, dir)
	}
	sort.Strings(mounts) // stable order (map iteration is random)
	for _, dir := range mounts {
		out = append(out, FolderInfo{Name: filepath.Base(dir), LocalDir: dir})
	}
	return out
}

// OpenFolder opens a local folder in the OS file manager.
func (a *App) OpenFolder(localDir string) { openPath(localDir) }

// OpenSyncFolder opens the account's sync folder (base dir) in Explorer.
func (a *App) OpenSyncFolder() {
	dir := a.GetBaseDir()
	_ = os.MkdirAll(dir, 0o755)
	openPath(dir)
}

// SearchItem is one unified-search hit for the flyout search bar.
type SearchItem struct {
	Title   string `json:"title"`   // file name
	Subline string `json:"subline"` // its folder
	Href    string `json:"href"`    // absolute URL to open it in the browser
}

// Search queries Nextcloud's unified file search for term (for the flyout).
func (a *App) Search(term string) []SearchItem {
	term = strings.TrimSpace(term)
	if a.eng == nil || len(term) < 2 {
		return nil
	}
	results, err := a.eng.SearchFiles(a.ctx, term, 12)
	if err != nil {
		slog.Debug("search failed", "term", term, "err", err)
		return nil
	}
	out := make([]SearchItem, 0, len(results))
	for _, r := range results {
		out = append(out, SearchItem{Title: r.Title, Subline: r.Subline, Href: a.absURL(r.ResourceURL)})
	}
	return out
}

// AppInfo is a Nextcloud app for the flyout (with its pinned state).
// Shortcut reports a Start-menu shortcut for the app (Windows). NOTE: the field
// reaches the frontend through the generated bindings WITHOUT a regen — the
// model constructor's Object.assign passes unknown JSON fields through — so
// keep it additive and read it untyped ((a as any).shortcut) in Svelte.
type AppInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Href     string `json:"href"`
	Icon     string `json:"icon"`
	Pinned   bool   `json:"pinned"`
	Shortcut bool   `json:"shortcut"`
}

// cachedApps returns the navigation-app list, served from a short-lived cache
// when possible. The flyout refetches on every open, so the cache is warm by
// the time a dock icon is clicked — app windows then appear instantly instead
// of waiting a server round-trip before anything shows.
func (a *App) cachedApps(ctx context.Context) ([]transport.App, error) {
	a.appsCacheMu.Lock()
	if a.appsCache != nil && time.Since(a.appsCacheAt) < 2*time.Minute {
		apps := a.appsCache
		a.appsCacheMu.Unlock()
		return apps, nil
	}
	a.appsCacheMu.Unlock()
	apps, err := a.eng.Apps(ctx)
	if err != nil {
		return nil, err
	}
	a.appsCacheMu.Lock()
	a.appsCache, a.appsCacheAt = apps, time.Now()
	a.appsCacheMu.Unlock()
	return apps, nil
}

// Apps returns the account's Nextcloud apps, marking which are pinned.
func (a *App) Apps() []AppInfo {
	if a.eng == nil {
		return nil
	}
	apps, err := a.cachedApps(context.Background())
	if err != nil {
		slog.Warn("apps fetch failed", "err", err)
		return nil
	}
	pinned := map[string]bool{}
	for _, id := range a.eng.PinnedApps() {
		pinned[id] = true
	}
	var st config.Settings
	if d, derr := config.Resolve(); derr == nil {
		st, _ = d.LoadSettings()
	}
	out := make([]AppInfo, 0, len(apps))
	for _, ap := range apps {
		name := ap.Name
		if name == "" {
			name = ap.ID
		}
		out = append(out, AppInfo{ID: ap.ID, Name: name, Href: a.absURL(ap.Href), Icon: a.absURL(ap.Icon),
			Pinned: pinned[ap.ID], Shortcut: shortcutExists(shortcutFileFor(st, ap.ID, name))})
	}
	return out
}

// PinApp / UnpinApp toggle an app's pinned state.
func (a *App) PinApp(id string) {
	if a.eng != nil {
		_ = a.eng.PinApp(id)
	}
}
func (a *App) UnpinApp(id string) {
	if a.eng != nil {
		_ = a.eng.UnpinApp(id)
	}
}

// OpenURL opens a link in the default browser — unless it carries one of the
// nimbo-app URI prefixes the flyout uses, which route to the app-window
// features instead. The prefixes ride the existing binding (adding a bound
// method would need a bindings regen, which has broken a release before).
func (a *App) OpenURL(href string) {
	if id, ok := strings.CutPrefix(href, "nimbo-app://"); ok {
		a.openAppWindow(id)
		return
	}
	if id, ok := strings.CutPrefix(href, "nimbo-app-shortcut://"); ok {
		a.toggleAppShortcut(id)
		return
	}
	openURL(a.absURL(href))
}

// absURL resolves a possibly-relative Nextcloud href (e.g. "/apps/files/")
// against the account's server URL. Absolute URLs are returned unchanged.
func (a *App) absURL(href string) string {
	if href == "" || strings.Contains(href, "://") {
		return href
	}
	if a.eng == nil {
		return href
	}
	return strings.TrimRight(a.eng.Account.ServerURL, "/") + "/" + strings.TrimLeft(href, "/")
}

// Quit exits the application.
func (a *App) Quit() {
	if a.app != nil {
		a.app.Quit()
	}
}

// buildTrayMenu constructs the tray right-click menu reflecting current state.
func (a *App) buildTrayMenu() *application.Menu {
	m := application.NewMenu()
	m.Add("Sync now").OnClick(func(*application.Context) { a.SyncNow() })

	ps := a.PauseInfo()
	if ps.Paused {
		if ps.Until != "" {
			info := m.Add("Paused until " + ps.Until)
			info.SetEnabled(false)
		} else {
			info := m.Add("Paused")
			info.SetEnabled(false)
		}
		m.Add("Resume syncing").OnClick(func(*application.Context) { a.Resume() })
	} else {
		sub := m.AddSubmenu("Pause syncing")
		sub.Add("For 1 hour").OnClick(func(*application.Context) { a.PauseFor(60) })
		sub.Add("For 4 hours").OnClick(func(*application.Context) { a.PauseFor(240) })
		sub.Add("Until tomorrow").OnClick(func(*application.Context) { a.PauseUntilTomorrow() })
		sub.Add("Indefinitely").OnClick(func(*application.Context) { a.PauseFor(0) })
	}

	m.AddSeparator()
	if a.eng != nil {
		if pairs, _ := a.eng.Pairs(); len(pairs) == 1 {
			d := pairs[0].LocalDir
			m.Add("Open " + filepath.Base(d)).OnClick(func(*application.Context) { openPath(d) })
		} else if len(pairs) > 1 {
			sub := m.AddSubmenu("Open folder")
			for _, p := range pairs {
				d := p.LocalDir
				sub.Add(filepath.Base(d)).OnClick(func(*application.Context) { openPath(d) })
			}
		}
	}
	m.Add("Sync status").OnClick(func(*application.Context) { a.OpenStatus() })
	m.Add("Settings").OnClick(func(*application.Context) { a.OpenSettings() })
	m.AddSeparator()
	m.Add("Quit Nimbo").OnClick(func(*application.Context) { a.Quit() })
	return m
}

// rebuildTrayMenu refreshes the tray menu (pause label, folder list) on the main
// thread. Safe to call from any goroutine.
func (a *App) rebuildTrayMenu() {
	if a.tray == nil {
		return
	}
	application.InvokeAsync(func() { a.tray.SetMenu(a.buildTrayMenu()) })
}

// --- Status window data + actions ---

// ActivityItem is one recent sync operation.
type ActivityItem struct {
	Time       string `json:"time"`
	Kind       string `json:"kind"`
	Path       string `json:"path"`       // pair-relative path
	RemotePath string `json:"remotePath"` // files-root-relative path (for sharing)
	Err        string `json:"err"`
	Account    string `json:"account"` // owning account's user name (set when several accounts are configured)
}

// RecentActivity returns the recent sync operations (newest first) across ALL
// syncing accounts, each item tagged with its account when more than one is
// configured.
func (a *App) RecentActivity() []ActivityItem {
	if a.eng == nil {
		return nil
	}
	multi := len(a.secondaries) > 0
	type timed struct {
		item ActivityItem
		at   time.Time
	}
	var all []timed
	collect := func(eng *agent.Engine) {
		// Map each pair's local dir to its remote root so activity (which is
		// pair-relative) can be resolved to a shareable remote path.
		roots := map[string]string{}
		if pairs, err := eng.Pairs(); err == nil {
			for _, p := range pairs {
				roots[p.LocalDir] = strings.Trim(p.RemoteRoot, "/")
			}
		}
		acct := ""
		if multi {
			acct = eng.Account.LoginName
		}
		for _, e := range eng.Recorder().Recent() {
			rp := strings.Trim(e.Path, "/")
			if root := roots[e.Local]; root != "" {
				rp = root + "/" + rp
			}
			all = append(all, timed{
				at:   e.Time,
				item: ActivityItem{Time: e.Time.Format("15:04:05"), Kind: e.Kind, Path: e.Path, RemotePath: rp, Err: e.Err, Account: acct},
			})
		}
	}
	collect(a.eng)
	for _, se := range a.secondaries {
		collect(se.eng)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].at.After(all[j].at) })
	if len(all) > 200 {
		all = all[:200]
	}
	out := make([]ActivityItem, len(all))
	for i, t := range all {
		out[i] = t.item
	}
	return out
}

// ConflictItem is a pending conflict awaiting a choice, with both versions'
// size and modified time so the user can decide.
type ConflictItem struct {
	LocalDir     string `json:"localDir"`
	Path         string `json:"path"`
	Kind         string `json:"kind"`
	LocalExists  bool   `json:"localExists"`
	RemoteExists bool   `json:"remoteExists"`
	LocalSize    int64  `json:"localSize"`
	LocalMTime   string `json:"localMTime"`  // "YYYY-MM-DD HH:MM" or ""
	RemoteSize   int64  `json:"remoteSize"`
	RemoteMTime  string `json:"remoteMTime"`
}

// Conflicts returns the pending conflicts, each enriched with per-side metadata.
func (a *App) Conflicts() []ConflictItem {
	if a.eng == nil {
		return nil
	}
	const tf = "2006-01-02 15:04"
	var out []ConflictItem
	for _, c := range a.eng.PendingConflicts() {
		it := ConflictItem{
			LocalDir: c.LocalDir, Path: c.Path, Kind: c.Kind,
			LocalExists: c.LocalExists, RemoteExists: c.RemoteExists,
			LocalSize: c.LocalSize, RemoteSize: c.RemoteSize,
		}
		// Metadata was captured when the conflict was detected, so the list opens
		// instantly with no per-conflict server round trip.
		if !c.LocalMTime.IsZero() {
			it.LocalMTime = c.LocalMTime.Format(tf)
		}
		if !c.RemoteMTime.IsZero() {
			it.RemoteMTime = c.RemoteMTime.Format(tf)
		}
		out = append(out, it)
	}
	return out
}

// conflictPreviewMax is how many bytes of each side we sample for the preview —
// enough to eyeball a text file, small enough to fetch instantly (we close the
// remote stream after this, so a big file is never fully downloaded).
const conflictPreviewMax = 16 * 1024

// SidePreview is one side (local or server) of a conflict's content preview.
type SidePreview struct {
	Exists    bool   `json:"exists"`
	Size      int64  `json:"size"`
	IsText    bool   `json:"isText"`
	Preview   string `json:"preview"`   // sampled text (UTF-8), empty for binary/missing
	Truncated bool   `json:"truncated"` // file is larger than the sample
	Note      string `json:"note"`      // why there's no text preview (binary / deleted / error)
}

// ConflictPreview holds both versions' content previews so the user can see what
// they're choosing between, not just sizes and timestamps.
type ConflictPreview struct {
	Local  SidePreview `json:"local"`
	Remote SidePreview `json:"remote"`
}

// ConflictPreview returns a short text sample of each side of a pending conflict.
// Binary files report a note instead of garbled text; the remote stream is closed
// after the sample, so large files aren't fully downloaded just to preview them.
func (a *App) ConflictPreview(localDir, path string) ConflictPreview {
	var out ConflictPreview
	if a.eng == nil {
		return out
	}
	var rroot string
	var lEx, rEx, found bool
	for _, c := range a.eng.PendingConflicts() {
		if c.LocalDir == localDir && c.Path == path {
			rroot, lEx, rEx, found = c.RemoteRoot, c.LocalExists, c.RemoteExists, true
			break
		}
	}
	if !found {
		return out
	}

	if lEx {
		lp := filepath.Join(localDir, filepath.FromSlash(path))
		if fi, err := os.Stat(lp); err == nil && !fi.IsDir() {
			out.Local.Exists = true
			out.Local.Size = fi.Size()
			if f, err := os.Open(lp); err == nil {
				b, _ := io.ReadAll(io.LimitReader(f, conflictPreviewMax))
				f.Close()
				fillPreview(&out.Local, b, fi.Size())
			} else {
				out.Local.Note = "couldn't read the local file"
			}
		}
	} else {
		out.Local.Note = "deleted here"
	}

	if rEx {
		rp := strings.Trim(rroot, "/") + "/" + path
		if rc, hdr, err := a.eng.Client().Get(a.ctx, rp); err == nil {
			b, _ := io.ReadAll(io.LimitReader(rc, conflictPreviewMax))
			rc.Close() // abort the rest of the transfer — preview only
			out.Remote.Exists = true
			if cl := hdr.Get("Content-Length"); cl != "" {
				fmt.Sscan(cl, &out.Remote.Size)
			}
			if out.Remote.Size == 0 {
				out.Remote.Size = int64(len(b))
			}
			fillPreview(&out.Remote, b, out.Remote.Size)
		} else {
			out.Remote.Note = "couldn't fetch the server file"
		}
	} else {
		out.Remote.Note = "deleted on server"
	}
	return out
}

// fillPreview classifies a sampled byte slice and fills a SidePreview: a text
// sample if it looks like text, otherwise a "binary" note.
func fillPreview(s *SidePreview, b []byte, fullSize int64) {
	if looksLikeText(b) {
		s.IsText = true
		s.Preview = string(b)
		s.Truncated = fullSize > int64(len(b))
	} else {
		s.Note = "binary file — no text preview"
	}
}

// looksLikeText reports whether a sample is plausibly UTF-8 text: no NUL bytes and
// valid UTF-8 (tolerating a rune cut off by the sample boundary).
func looksLikeText(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	if bytes.IndexByte(b, 0) >= 0 {
		return false
	}
	if utf8.Valid(b) {
		return true
	}
	for k := 1; k <= 3 && k < len(b); k++ { // sample may have sliced mid-rune
		if utf8.Valid(b[:len(b)-k]) {
			return true
		}
	}
	return false
}

// conflictPolicy maps a settings string to the engine policy ("" = Ask).
func conflictPolicy(s string) transfer.ConflictPolicy {
	switch s {
	case "keepboth":
		return transfer.PolicyAuto
	case "newest":
		return transfer.PolicyNewest
	default:
		return transfer.PolicyAsk
	}
}

// GetConflictPolicy returns the saved default conflict policy.
func (a *App) GetConflictPolicy() string {
	if d, err := config.Resolve(); err == nil {
		if s, e := d.LoadSettings(); e == nil && s.ConflictPolicy != "" {
			return s.ConflictPolicy
		}
	}
	return "ask"
}

// SetConflictPolicy saves and applies the default conflict policy.
func (a *App) SetConflictPolicy(policy string) {
	if d, err := config.Resolve(); err == nil {
		if s, e := d.LoadSettings(); e == nil {
			s.ConflictPolicy = policy
			_ = d.SaveSettings(s)
		}
	}
	if a.eng != nil {
		a.eng.SetConflictPolicy(conflictPolicy(policy))
		a.eng.TriggerSync() // re-evaluate any deferred conflicts under the new policy
	}
}

// GetAllowedFilenames returns the filenames the user permits even when normally
// blocked (the builtin web-config block — .htaccess/.htpasswd/.user.ini — or the
// server's forbidden list).
func (a *App) GetAllowedFilenames() []string {
	if d, err := config.Resolve(); err == nil {
		if s, e := d.LoadSettings(); e == nil {
			return s.AllowedFilenames
		}
	}
	return nil
}

// SetAllowedFilenames persists the allow-list (trimmed, de-duped) and applies it
// live: the running engine rebuilds its forbidden-name matcher and re-syncs, so a
// change takes effect immediately instead of on next launch.
func (a *App) SetAllowedFilenames(names []string) {
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || seen[strings.ToLower(n)] {
			continue
		}
		seen[strings.ToLower(n)] = true
		out = append(out, n)
	}
	if d, err := config.Resolve(); err == nil {
		if s, e := d.LoadSettings(); e == nil {
			s.AllowedFilenames = out
			_ = d.SaveSettings(s)
		}
	}
	if a.eng != nil {
		a.eng.SetAllowedFilenames(out)
	}
}

// GetIgnorePatterns returns the global ignore patterns (one glob per entry) that
// exclude matching paths from every sync pair, on top of Nimbo's built-in
// defaults (node_modules, .git, OS/editor cruft, …).
func (a *App) GetIgnorePatterns() []string {
	if d, err := config.Resolve(); err == nil {
		if pats, e := d.LoadIgnore(); e == nil {
			return pats
		}
	}
	return nil
}

// SetIgnorePatterns persists the global ignore patterns (trimmed, comments/blank
// lines dropped) and triggers a sync so the change applies promptly. Ignored
// paths are left untouched on both sides (not synced, not deleted).
func (a *App) SetIgnorePatterns(patterns []string) {
	var out []string
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" || strings.HasPrefix(p, "#") {
			continue
		}
		out = append(out, p)
	}
	if d, err := config.Resolve(); err == nil {
		_ = d.SaveIgnore(out)
	}
	if a.eng != nil {
		a.eng.TriggerSync() // re-evaluate with the new patterns now
	}
}

// DiagnosticsDTO is a connection/sync health snapshot for the health panel.
type DiagnosticsDTO struct {
	ServerURL     string `json:"serverURL"`
	ServerVersion string `json:"serverVersion"`
	Account       string `json:"account"`
	PushAvailable bool   `json:"pushAvailable"`
	PushConnected bool   `json:"pushConnected"`
	PushUptime    string `json:"pushUptime"`
	LastStatus    string `json:"lastStatus"`
	LastSync      string `json:"lastSync"`
}

// Diagnostics returns Nimbo's current health (no network calls) for the UI.
func (a *App) Diagnostics() DiagnosticsDTO {
	if a.eng == nil {
		return DiagnosticsDTO{LastStatus: "Not signed in"}
	}
	d := a.eng.Diagnostics()
	dto := DiagnosticsDTO{
		ServerURL:     d.ServerURL,
		ServerVersion: d.ServerVersion,
		Account:       d.Account,
		PushAvailable: d.PushAvailable,
		PushConnected: d.PushConnected,
		LastStatus:    d.LastStatus,
		LastSync:      "—",
	}
	if d.PushConnected && !d.PushSince.IsZero() {
		dto.PushUptime = humanDuration(time.Since(d.PushSince))
	}
	if !d.LastSyncAt.IsZero() {
		dto.LastSync = humanAgo(time.Since(d.LastSyncAt))
	}
	return dto
}

// humanDuration renders an uptime like "2h 14m" / "9m" / "45s".
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// humanAgo renders an elapsed time like "just now" / "12s ago" / "3m ago".
func humanAgo(d time.Duration) string {
	switch {
	case d < 5*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
}

// ResolveConflict settles a conflict: choice is "local", "remote", or "both".
func (a *App) ResolveConflict(localDir, path, choice string) {
	if a.eng == nil {
		return
	}
	var c transfer.Choice
	switch choice {
	case "local":
		c = transfer.ChoiceKeepLocal
	case "remote":
		c = transfer.ChoiceKeepRemote
	default:
		c = transfer.ChoiceKeepBoth
	}
	for _, it := range a.eng.PendingConflicts() {
		if it.LocalDir == localDir && it.Path == path {
			if err := a.eng.ResolveConflict(context.Background(), it, c); err != nil {
				slog.Warn("resolve conflict failed", "err", err)
			}
			return
		}
	}
}

// NotifAction is a button on a notification.
type NotifAction struct {
	Label string `json:"label"`
}

// NotifItem is a Nextcloud notification.
type NotifItem struct {
	ID      int           `json:"id"`
	App     string        `json:"app"`
	Subject string        `json:"subject"`
	Message string        `json:"message"`
	Link    string        `json:"link"`
	Actions []NotifAction `json:"actions"`
}

// NotificationList returns the current notifications.
func (a *App) NotificationList() []NotifItem {
	if a.eng == nil {
		return nil
	}
	var out []NotifItem
	for _, n := range a.eng.Notifier().List() {
		acts := make([]NotifAction, 0, len(n.Actions))
		for _, ac := range n.Actions {
			acts = append(acts, NotifAction{Label: ac.Label})
		}
		out = append(out, NotifItem{ID: n.ID, App: n.App, Subject: n.Subject, Message: n.Message, Link: n.Link, Actions: acts})
	}
	return out
}

// DismissNotification removes a notification by ID.
func (a *App) DismissNotification(id int) {
	if a.eng != nil {
		_ = a.eng.DismissNotification(context.Background(), id)
	}
}

// DismissAllNotifications clears every notification for the active account.
func (a *App) DismissAllNotifications() {
	if a.eng != nil {
		_ = a.eng.DismissAllNotifications(context.Background())
	}
}

// DoNotificationAction runs a notification's action by label on the shown
// account (the Status-window UI binding).
func (a *App) DoNotificationAction(id int, label string) {
	a.doNotificationActionFor("", id, label)
}

// engineFor returns the engine for the given account id: the shown engine when
// the id is empty or matches it, otherwise the matching secondary; falls back to
// the shown engine.
func (a *App) engineFor(acct string) *agent.Engine {
	if a.eng != nil && (acct == "" || a.eng.Account.ID == acct) {
		return a.eng
	}
	if se, ok := a.secondaries[acct]; ok {
		return se.eng
	}
	return a.eng
}

// doNotificationActionFor runs a notification's action by label on the given
// account — so a toast raised by a secondary account acts on THAT account, not
// whichever one the UI happens to show.
func (a *App) doNotificationActionFor(acct string, id int, label string) {
	eng := a.engineFor(acct)
	if eng == nil {
		return
	}
	for _, n := range eng.Notifier().List() {
		if n.ID != id {
			continue
		}
		for _, ac := range n.Actions {
			if ac.Label == label {
				_ = eng.DoNotificationAction(context.Background(), ac)
				a.emit("notifications")
				return
			}
		}
	}
}

// BlockedItem is a file that can't sync (server-forbidden name), or — when
// Escaping is set — a synthetic row for an extension currently being escaped
// (its "Ext" is what a "stop escaping" action removes).
type BlockedItem struct {
	Abs       string `json:"abs"`
	Path      string `json:"path"`
	Reason    string `json:"reason"`
	Ext       string `json:"ext"`       // file extension (for the escape opt-in label)
	Escapable bool   `json:"escapable"` // opting Ext in would let this file sync
	Escaping  bool   `json:"escaping"`  // synthetic row: this Ext is currently escaped
}

// BlockedList returns files that can't sync, plus a synthetic row per
// currently-escaped extension so the UI can offer to stop escaping it.
func (a *App) BlockedList() []BlockedItem {
	if a.eng == nil {
		return nil
	}
	var out []BlockedItem
	for _, b := range a.eng.BlockedFiles() {
		base := filepath.Base(b.Path)
		out = append(out, BlockedItem{
			Abs: b.Abs, Path: b.Path, Reason: b.Reason,
			Ext:       filepath.Ext(base),
			Escapable: a.eng.CanEscape(base),
		})
	}
	for _, ext := range a.eng.EscapedExtensions() {
		out = append(out, BlockedItem{Path: ext, Ext: ext, Escaping: true,
			Reason: "syncing on the server under a renamed copy"})
	}
	return out
}

// BlacklistBlocked stops attempting to sync a blocked file.
func (a *App) BlacklistBlocked(abs string) {
	if a.eng != nil {
		_ = a.eng.BlacklistPath(abs)
	}
}

// escape-control sentinels smuggled through RenameBlocked's newName argument, so
// the opt-in / opt-out actions need no new Wails binding (a bindings regen once
// broke a release). A real rename target is a basename and can never contain a
// slash, so these can't collide with one.
const (
	escapeOptIn  = "//escape"    // opt in the blocked file's extension
	escapeOptOut = "//unescape:" // + <ext>: stop escaping that extension
)

// RenameBlocked renames a blocked file so it can sync — or, when newName is an
// escape-control sentinel, opts the file's extension into (or out of) escaping.
func (a *App) RenameBlocked(abs, newName string) {
	if a.eng == nil {
		return
	}
	switch {
	case newName == escapeOptIn:
		if err := a.eng.EnableEscaping(filepath.Ext(filepath.Base(abs))); err != nil {
			notify.Toast(brand.Current.Name, "Couldn't start syncing those files: "+err.Error(), "")
		}
	case strings.HasPrefix(newName, escapeOptOut):
		ext := strings.TrimPrefix(newName, escapeOptOut)
		go func() { // server round-trips (scan + deletes); don't block the UI thread
			if n, err := a.eng.DisableEscaping(a.ctx, ext); err != nil {
				notify.Toast(brand.Current.Name, "Couldn't stop escaping "+ext+": "+err.Error(), "")
			} else {
				slog.Info("stopped escaping", "ext", ext, "serverCopiesRemoved", n)
			}
			a.emit("blocked")
		}()
	default:
		_ = a.eng.RenameBlocked(abs, newName)
	}
}

// DeleteBlocked deletes a blocked file from disk (it can't sync because of its name).
func (a *App) DeleteBlocked(abs string) {
	if a.eng != nil {
		_ = a.eng.DeleteBlocked(abs)
	}
}

// DeleteAllBlocked deletes every blocked file from disk; returns the count removed.
func (a *App) DeleteAllBlocked() int {
	if a.eng == nil {
		return 0
	}
	n, _ := a.eng.DeleteAllBlocked()
	return n
}

// OpenStatus opens (or focuses) the status window.
func (a *App) OpenStatus() { a.openStatus("") }

// OpenStatusTab opens (or focuses) the status window on a specific tab
// ("activity", "conflicts", "notifications", "blocked", "trash").
func (a *App) OpenStatusTab(tab string) { a.openStatus(tab) }

// InitialStatusTab returns (and clears) the tab the status window should open
// on; the status view calls it once on mount. "" means the default tab.
func (a *App) InitialStatusTab() string {
	t := a.statusTab
	a.statusTab = ""
	return t
}

func (a *App) openStatus(tab string) {
	a.statusTab = tab
	if a.statusWin != nil {
		a.statusWin.Show()
		a.statusWin.Focus()
		if tab != "" && a.app != nil {
			a.app.Event.Emit("status-tab", tab) // already open → switch its tab
		}
		return
	}
	a.statusWin = a.app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:   "status",
		Title:  brand.Current.Name,
		Width:  760,
		Height: 580,
		URL:    "/#status",
	})
	a.statusWin.OnWindowEvent(events.Common.WindowClosing, func(*application.WindowEvent) { a.statusWin = nil })
}

// --- Sharing ---

// ShareDTO is an existing share on the target path.
type ShareDTO struct {
	ID          string `json:"id"`
	Type        int    `json:"type"`      // 0 = user, 3 = public link
	ShareWith   string `json:"shareWith"` // username (user shares)
	URL         string `json:"url"`       // public-link URL
	Permissions int    `json:"permissions"`
	Expiration  string `json:"expiration"`
}

// OpenShare opens (or focuses) the share window for a remote, files-root-relative
// path (e.g. "Photos/trip.jpg").
// bringToFront shows a window and forces it to the foreground. A brief
// always-on-top toggle defeats Windows' foreground-stealing lock when the window
// is opened from a background activation (e.g. an Explorer context-menu verb).
func bringToFront(w *application.WebviewWindow) {
	if w == nil {
		return
	}
	w.Show()
	w.SetAlwaysOnTop(true)
	w.Focus()
	w.SetAlwaysOnTop(false)
}

func (a *App) OpenShare(remotePath string) {
	a.sharePath = strings.Trim(remotePath, "/")
	if a.shareWin != nil {
		bringToFront(a.shareWin)
		a.emit("share:target") // tell the window to reload for the new path
		return
	}
	a.shareWin = a.app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name: "share", Title: brand.Current.Name+" — Share", Width: 480, Height: 560, URL: "/#share",
	})
	a.shareWin.OnWindowEvent(events.Common.WindowClosing, func(*application.WindowEvent) { a.shareWin = nil })
	bringToFront(a.shareWin)
}

// ShareTarget returns the path the share window is acting on.
func (a *App) ShareTarget() string { return a.sharePath }

// ShareList returns the existing shares on the target path.
func (a *App) ShareList() []ShareDTO {
	if a.eng == nil || a.sharePath == "" {
		return nil
	}
	shares, err := a.eng.ListShares(a.ctx, a.sharePath)
	if err != nil {
		slog.Warn("list shares failed", "path", a.sharePath, "err", err)
		return nil
	}
	out := make([]ShareDTO, 0, len(shares))
	for _, s := range shares {
		out = append(out, ShareDTO{
			ID: s.ID.String(), Type: s.ShareType, ShareWith: s.ShareWith,
			URL: s.URL, Permissions: s.Permissions, Expiration: s.Expiration,
		})
	}
	return out
}

func sharePerms(allowEdit bool) int {
	if allowEdit {
		return transport.PermRead | transport.PermUpdate | transport.PermCreate | transport.PermDelete
	}
	return transport.PermRead
}

// CreatePublicLink creates a public-link share on the target path. password and
// expiration ("YYYY-MM-DD") are optional. Returns "" on success or an error.
func (a *App) CreatePublicLink(password, expiration string, allowEdit bool) string {
	if a.eng == nil {
		return "not signed in"
	}
	_, err := a.eng.CreatePublicLink(a.ctx, a.sharePath, transport.PublicLinkOptions{
		Password: password, Permissions: sharePerms(allowEdit), Expiration: expiration,
	})
	if err != nil {
		return err.Error()
	}
	notify.Toast("Public link created", filepath.Base(a.sharePath)+" now has a shareable link", "")
	return ""
}

// CreateUserShare shares the target path with another user. Returns "" on success.
func (a *App) CreateUserShare(user string, allowEdit bool) string {
	if a.eng == nil {
		return "not signed in"
	}
	if strings.TrimSpace(user) == "" {
		return "enter a username"
	}
	if _, err := a.eng.CreateUserShare(a.ctx, a.sharePath, strings.TrimSpace(user), sharePerms(allowEdit)); err != nil {
		return err.Error()
	}
	notify.Toast("Shared", filepath.Base(a.sharePath)+" shared with "+strings.TrimSpace(user), "")
	return ""
}

// DeleteShare removes a share by ID. Returns "" on success.
func (a *App) DeleteShare(id string) string {
	if a.eng == nil {
		return "not signed in"
	}
	if err := a.eng.DeleteShare(a.ctx, id); err != nil {
		return err.Error()
	}
	return ""
}

// CopyToClipboard copies text (e.g. a share link) to the system clipboard.
func (a *App) CopyToClipboard(text string) {
	if a.app != nil {
		a.app.Clipboard.SetText(text)
	}
}

// VersionDTO is a stored version of the version-window's target file.
type VersionDTO struct {
	Href     string `json:"href"`
	Modified string `json:"modified"`
	Size     int64  `json:"size"`
}

// OpenVersions opens (or focuses) the version-history window for a remote,
// files-root-relative path.
func (a *App) OpenVersions(remotePath string) {
	a.versionPath = strings.Trim(remotePath, "/")
	if a.versionsWin != nil {
		bringToFront(a.versionsWin)
		a.emit("versions:target")
		return
	}
	a.versionsWin = a.app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name: "versions", Title: brand.Current.Name+" — Version history", Width: 460, Height: 480, URL: "/#versions",
	})
	a.versionsWin.OnWindowEvent(events.Common.WindowClosing, func(*application.WindowEvent) { a.versionsWin = nil })
	bringToFront(a.versionsWin)
}

// VersionTarget returns the path the version window is acting on.
func (a *App) VersionTarget() string { return a.versionPath }

// VersionList returns the version history of the version-window's target file.
func (a *App) VersionList() []VersionDTO {
	if a.eng == nil || a.versionPath == "" {
		return nil
	}
	ent, ok, err := a.eng.StatRemote(a.ctx, a.versionPath)
	if err != nil || !ok || ent.FileID == "" {
		return nil
	}
	vs, err := a.eng.Versions(a.ctx, ent.FileID)
	if err != nil {
		slog.Warn("list versions failed", "path", a.versionPath, "err", err)
		return nil
	}
	out := make([]VersionDTO, 0, len(vs))
	for _, v := range vs {
		out = append(out, VersionDTO{Href: v.Href, Modified: v.Modified.Format("2006-01-02 15:04"), Size: v.Size})
	}
	return out
}

// RestoreVersion makes a stored version current. Returns "" on success.
func (a *App) RestoreVersion(href string) string {
	if a.eng == nil {
		return "not signed in"
	}
	if err := a.eng.RestoreVersion(a.ctx, href); err != nil {
		return err.Error()
	}
	notify.Toast("Version restored", filepath.Base(a.versionPath)+" was rolled back", "")
	return ""
}

// --- Trashbin ---

// TrashDTO is a deleted item in the Nextcloud trashbin.
type TrashDTO struct {
	Href             string `json:"href"`
	Name             string `json:"name"`
	OriginalLocation string `json:"originalLocation"`
	DeletedAt        string `json:"deletedAt"`
	Size             int64  `json:"size"`
	IsDir            bool   `json:"isDir"`
}

// TrashList returns the items currently in the trashbin.
func (a *App) TrashList() []TrashDTO {
	if a.eng == nil {
		return nil
	}
	items, err := a.eng.Trash(a.ctx)
	if err != nil {
		slog.Warn("trash list failed", "err", err)
		return nil
	}
	// Newest first, and cap the list so a huge trashbin doesn't choke the UI.
	sort.Slice(items, func(i, j int) bool { return items[i].DeletedAt.After(items[j].DeletedAt) })
	const max = 500
	if len(items) > max {
		items = items[:max]
	}
	out := make([]TrashDTO, 0, len(items))
	for _, t := range items {
		d := ""
		if !t.DeletedAt.IsZero() {
			d = t.DeletedAt.Format("2006-01-02 15:04")
		}
		out = append(out, TrashDTO{
			Href: t.Href, Name: t.Name, OriginalLocation: t.OriginalLocation,
			DeletedAt: d, Size: t.Size, IsDir: t.IsDir,
		})
	}
	return out
}

// RestoreTrash restores a trashed item by href. Returns "" on success.
func (a *App) RestoreTrash(href string) string {
	if a.eng == nil {
		return "not signed in"
	}
	if err := a.eng.RestoreTrash(a.ctx, href); err != nil {
		return err.Error()
	}
	return ""
}

// DeleteTrash permanently deletes a trashed item by href. Returns "" on success.
func (a *App) DeleteTrash(href string) string {
	if a.eng == nil {
		return "not signed in"
	}
	if err := a.eng.DeleteTrash(a.ctx, href); err != nil {
		return err.Error()
	}
	return ""
}

// --- Explorer "Share with Nimbo" integration ---

// argValue extracts the value of "<flag> <value>" or "<flag>=<value>" from argv.
func argValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(a, flag+"=") {
			return strings.TrimPrefix(a, flag+"=")
		}
	}
	return ""
}

// dispatchToastActivation routes a clicked toast (or toast button) to an action.
// The activation string is the URL-query we put in the toast's `launch` / button
// `arguments` (see the toast-raising sites). It arrives on a COM RPC thread, so
// all UI work is marshalled to the UI thread via InvokeAsync — mirroring
// onSecondInstance.
func (a *App) dispatchToastActivation(args string) {
	slog.Info("toast activated", "args", args)
	v, err := url.ParseQuery(args)
	if err != nil {
		application.InvokeAsync(func() { a.openStatus("") }) // unparseable → just surface the app
		return
	}
	switch v.Get("action") {
	case "login":
		application.InvokeAsync(func() { a.showLogin() })
	case "update":
		application.InvokeAsync(func() {
			// On success the app restarts; a non-empty return is an error or
			// "already up to date" — surface it (from a toast nobody sees the
			// string ApplyUpdate returns to the Settings UI).
			if msg := a.ApplyUpdate(); msg != "" {
				notify.Toast(brand.Current.Name+" update", msg, "")
			}
		})
	case "notifications":
		application.InvokeAsync(func() { a.openStatus("notifications") })
	case "settings":
		application.InvokeAsync(func() { a.openStatus("settings") })
	case "notify": // a Nextcloud notification's Accept/Decline button
		acct := v.Get("acct")
		id, _ := strconv.Atoi(v.Get("id"))
		label := v.Get("label")
		application.InvokeAsync(func() { a.doNotificationActionFor(acct, id, label) })
	default: // "status", the test buttons, empty/unknown → surface the app
		application.InvokeAsync(func() { a.openStatus("") })
	}
}

// onSecondInstance handles a second launch (e.g. from the Explorer context
// menu): it shares or shows versions for the requested path in this instance.
func (a *App) onSecondInstance(args []string) {
	if id := argValue(args, "--app"); id != "" {
		go a.openAppWindow(id) // not InvokeAsync — the app-list fetch mustn't block the UI thread
		return
	}
	if p := argValue(args, "--share"); p != "" {
		application.InvokeAsync(func() { a.shareLocalPath(p) })
		return
	}
	if p := argValue(args, "--versions"); p != "" {
		application.InvokeAsync(func() { a.versionsLocalPath(p) })
		return
	}
	if p := argValue(args, "--keep"); p != "" {
		application.InvokeAsync(func() { a.keepLocalPath(p) })
		return
	}
	if p := argValue(args, "--free"); p != "" {
		application.InvokeAsync(func() { a.freeLocalPath(p) })
	}
}

// remotePathForLocal maps a local path to its files-root-relative remote path,
// checking on-demand mounts first (in on-demand mode there are no live pairs)
// then live sync pairs. Used by Share / Version history.
func (a *App) remotePathForLocal(localAbs string) (string, bool) {
	c := filepath.Clean(localAbs)
	for dir, m := range a.onDemandMounts {
		rel, err := filepath.Rel(filepath.Clean(dir), c)
		if err != nil {
			continue
		}
		rel = filepath.ToSlash(rel)
		if rel == ".." || strings.HasPrefix(rel, "../") {
			continue
		}
		root := strings.Trim(m.remoteRoot, "/")
		switch {
		case rel == ".":
			return root, true
		case root == "":
			return rel, true
		default:
			return root + "/" + rel, true
		}
	}
	if a.eng != nil {
		return a.eng.RemotePathFor(localAbs)
	}
	return "", false
}

// onDemandMountFor returns the mount root containing localAbs, if any.
func (a *App) onDemandMountFor(localAbs string) (string, bool) {
	c := filepath.Clean(localAbs)
	for dir := range a.onDemandMounts {
		rel, err := filepath.Rel(filepath.Clean(dir), c)
		if err != nil {
			continue
		}
		if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))) {
			return dir, true
		}
	}
	return "", false
}

func (a *App) warnNotOnDemand(localAbs string) {
	if a.app != nil {
		a.app.Dialog.Warning().
			SetTitle("Not an on-demand item").
			SetMessage(filepath.Base(localAbs) + " isn't inside an on-demand (virtual files) folder.").
			Show()
	}
}

// keepLocalPath pins a file/folder so it's always kept on the device (pinning
// also triggers download of any online-only content).
func (a *App) keepLocalPath(localAbs string) {
	if localAbs == "" {
		return
	}
	if _, ok := a.onDemandMountFor(localAbs); !ok {
		if a.onDemandMounts == nil { // mounts not ready yet — queue it
			a.pendingKeep = localAbs
			return
		}
		a.warnNotOnDemand(localAbs)
		return
	}
	fi, err := os.Stat(localAbs)
	recurse := err == nil && fi.IsDir()
	if err := cfapi.SetPinState(localAbs, true, recurse); err != nil {
		slog.Warn("keep on device", "path", localAbs, "err", err)
	} else {
		slog.Info("kept on device", "path", localAbs)
	}
}

// freeLocalPath marks a file/folder online-only and drops its local content to
// reclaim disk space (the placeholder stays; it re-downloads on next open).
func (a *App) freeLocalPath(localAbs string) {
	if localAbs == "" {
		return
	}
	if _, ok := a.onDemandMountFor(localAbs); !ok {
		if a.onDemandMounts == nil {
			a.pendingFree = localAbs
			return
		}
		a.warnNotOnDemand(localAbs)
		return
	}
	fi, err := os.Stat(localAbs)
	isDir := err == nil && fi.IsDir()
	_ = cfapi.SetPinState(localAbs, false, isDir)
	if isDir {
		go a.dehydrateTree(localAbs)
	} else if derr := cfapi.Dehydrate(localAbs); derr != nil {
		slog.Debug("dehydrate", "path", localAbs, "err", derr)
	}
	slog.Info("freed up space", "path", localAbs)
}

// dehydrateTree drops local content for every hydrated file under root
// (best-effort; already-online-only files error harmlessly).
func (a *App) dehydrateTree(root string) {
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		_ = cfapi.Dehydrate(p)
		return nil
	})
}

// versionsLocalPath maps a local file to its remote path and opens the version
// history window. Queued if the engine isn't ready yet.
func (a *App) versionsLocalPath(localAbs string) {
	if localAbs == "" {
		return
	}
	if a.eng == nil {
		a.pendingVersions = localAbs
		return
	}
	remote, ok := a.remotePathForLocal(localAbs)
	if !ok {
		if a.app != nil {
			a.app.Dialog.Warning().
				SetTitle("No version history").
				SetMessage(filepath.Base(localAbs) + " isn't inside a folder synced by Nimbo.").
				Show()
		}
		return
	}
	a.OpenVersions(remote)
}

// shareLocalPath maps a local file/folder to its remote path and opens the
// share window. If the engine isn't ready yet, the request is queued.
func (a *App) shareLocalPath(localAbs string) {
	if localAbs == "" {
		return
	}
	if a.eng == nil {
		a.pendingShare = localAbs
		return
	}
	remote, ok := a.remotePathForLocal(localAbs)
	if !ok {
		if a.app != nil {
			a.app.Dialog.Warning().
				SetTitle("Can't share").
				SetMessage(filepath.Base(localAbs) + " isn't inside a folder synced by Nimbo.").
				Show()
		}
		return
	}
	a.OpenShare(remote)
}

// ShellMenuSupported reports whether the Explorer integration is available.
func (a *App) ShellMenuSupported() bool { return shellmenu.Supported() }

// ShellMenuEnabled reports whether the Explorer "Share" entry is registered.
func (a *App) ShellMenuEnabled() bool { return shellmenu.Enabled() }

// SetShellMenu registers or removes the Explorer "Share with Nimbo" entry.
func (a *App) SetShellMenu(on bool) string {
	if !on {
		return errString(shellmenu.Unregister())
	}
	exe, err := os.Executable()
	if err != nil {
		return err.Error()
	}
	return errString(shellmenu.Register(exe))
}

func errString(err error) string {
	if err != nil {
		return err.Error()
	}
	return ""
}

// --- Explorer navigation-pane (sidebar) entry ---

// SidebarSupported reports whether the Explorer sidebar entry is available.
func (a *App) SidebarSupported() bool { return shellns.Supported() }

// SidebarEnabled reports whether the Nimbo sidebar root is registered.
func (a *App) SidebarEnabled() bool { return shellns.Enabled() }

// SetSidebar adds or removes the Nimbo root in the Explorer navigation
// pane, pointing at the default sync location.
func (a *App) SetSidebar(on bool) string {
	if !on {
		return errString(shellns.Unregister())
	}
	target := a.GetBaseDir()
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err.Error()
	}
	icon, err := navIconPath()
	if err != nil {
		return err.Error()
	}
	return errString(shellns.Register(brand.Current.Name, target, icon))
}

// navIconPath writes the embedded app icon to the config dir and returns its
// path. Resolved via config so it lands in the brand's own folder and honours
// the packaged-vs-dev directory split (a hardcoded "nimbo" here used to let a
// dev run write into the packaged app's real config dir).
func navIconPath() (string, error) {
	d, err := config.Resolve()
	if err != nil {
		return "", err
	}
	p := filepath.Join(d.Config, "nimbo.ico")
	if err := os.WriteFile(p, navIconICO, 0o644); err != nil {
		return "", err
	}
	return p, nil
}

// --- Logging ---

// LogPath returns the application log file path.
func (a *App) LogPath() string {
	if d, err := config.Resolve(); err == nil {
		return d.LogFile()
	}
	return ""
}

// TailLog returns the last ~64 KB of the log file for the in-app viewer.
func (a *App) TailLog() string {
	p := a.LogPath()
	if p == "" {
		return ""
	}
	f, err := os.Open(p)
	if err != nil {
		return "(no log yet)"
	}
	defer f.Close()
	const max = 64 << 10
	fi, _ := f.Stat()
	trimmed := false
	if fi != nil && fi.Size() > max {
		_, _ = f.Seek(-max, io.SeekEnd)
		trimmed = true
	}
	b, _ := io.ReadAll(f)
	s := string(b)
	if trimmed { // drop the partial first line left by seeking mid-file
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
	}
	return s
}

// OpenLogFolder reveals the log directory in the file manager.
func (a *App) OpenLogFolder() {
	if p := a.LogPath(); p != "" {
		openPath(filepath.Dir(p))
	}
}

// OpenLogs opens (or focuses) the in-app log viewer window.
// ReportProblem bundles everything a bug report needs — the log files, a
// diagnostics snapshot, the version, and the (secret-free) settings — into a
// zip in Downloads, reveals it in Explorer, and opens the GitHub new-issue
// page so the user can attach it. Returns "" on success or an error message.
// No data leaves the machine until the user themselves attaches the file.
func (a *App) ReportProblem() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return err.Error()
	}
	out := filepath.Join(home, "Downloads", "nimbo-report-"+time.Now().Format("20060102-150405")+".zip")
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err.Error()
	}
	f, err := os.Create(out)
	if err != nil {
		return err.Error()
	}
	zw := zip.NewWriter(f)
	add := func(name string, data []byte) {
		w, werr := zw.Create(name)
		if werr == nil {
			_, _ = w.Write(data)
		}
	}
	// Version + environment.
	add("version.txt", []byte(fmt.Sprintf("%s %s\n%s/%s\n", brand.Current.Name, version, runtime.GOOS, runtime.GOARCH)))
	// Diagnostics snapshot (connection health, push state, last sync).
	if b, jerr := json.MarshalIndent(a.Diagnostics(), "", "  "); jerr == nil {
		add("diagnostics.json", b)
	}
	if d, derr := config.Resolve(); derr == nil {
		// Settings hold preferences and paths only — never credentials (those
		// live in the OS keychain).
		if b, rerr := os.ReadFile(filepath.Join(d.Config, "settings.json")); rerr == nil {
			add("settings.json", b)
		}
		// Every file in the logs dir (current + rotated).
		logDir := filepath.Dir(d.LogFile())
		if entries, rerr := os.ReadDir(logDir); rerr == nil {
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				if b, rerr := os.ReadFile(filepath.Join(logDir, e.Name())); rerr == nil {
					add("logs/"+e.Name(), b)
				}
			}
		}
	}
	if err := zw.Close(); err != nil {
		f.Close()
		return err.Error()
	}
	if err := f.Close(); err != nil {
		return err.Error()
	}
	revealPath(out)
	openURL("https://github.com/otherworld-dev/Nimbo/issues/new")
	return ""
}

func (a *App) OpenLogs() {
	if a.logsWin != nil {
		a.logsWin.Show()
		a.logsWin.Focus()
		return
	}
	a.logsWin = a.app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name: "logs", Title: brand.Current.Name+" — Logs", Width: 780, Height: 540, URL: "/#logs",
	})
	a.logsWin.OnWindowEvent(events.Common.WindowClosing, func(*application.WindowEvent) { a.logsWin = nil })
}

// Verbose reports whether debug logging is enabled.
func (a *App) Verbose() bool {
	if d, err := config.Resolve(); err == nil {
		if s, e := d.LoadSettings(); e == nil {
			return s.VerboseLog
		}
	}
	return false
}

// SetVerbose turns debug logging on/off (live) and persists the choice.
func (a *App) SetVerbose(on bool) {
	applog.SetVerbose(on)
	slog.Info("log verbosity changed", "verbose", on)
	if d, err := config.Resolve(); err == nil {
		if s, e := d.LoadSettings(); e == nil {
			s.VerboseLog = on
			_ = d.SaveSettings(s)
		}
	}
}

// --- Account & session ---

// AccountDTO describes the signed-in account for the settings UI.
type AccountDTO struct {
	SignedIn bool   `json:"signedIn"`
	User     string `json:"user"`
	Server   string `json:"server"`
}

// AccountInfo returns the current account, if signed in.
func (a *App) AccountInfo() AccountDTO {
	if a.eng == nil {
		return AccountDTO{}
	}
	return AccountDTO{SignedIn: true, User: a.eng.Account.LoginName, Server: a.eng.Account.ServerURL}
}

// AccountEntryDTO is one configured account in the account list.
type AccountEntryDTO struct {
	ID     string `json:"id"`
	User   string `json:"user"`
	Server string `json:"server"`
	Active bool   `json:"active"`
	Status string `json:"status"` // latest engine status line ("" if not running)
}

// ListAccounts returns every configured account. All of them sync
// concurrently; Active marks the primary one the windows and flyout show.
func (a *App) ListAccounts() []AccountEntryDTO {
	d, err := config.Resolve()
	if err != nil {
		return nil
	}
	st, err := account.LoadStore(d.AccountsFile())
	if err != nil {
		return nil
	}
	def, _ := st.Default()
	a.acctMu.Lock()
	defer a.acctMu.Unlock()
	out := make([]AccountEntryDTO, 0, len(st.Accounts))
	for _, ac := range st.Accounts {
		out = append(out, AccountEntryDTO{
			ID: ac.ID, User: ac.LoginName, Server: ac.ServerURL,
			Active: ac.ID == def.ID, Status: a.acctStatus[ac.ID],
		})
	}
	return out
}

// SwitchAccount makes the given account the primary one — the account the
// windows, flyout and folder settings show. ALL configured accounts keep
// syncing regardless (the previous primary continues as a background
// secondary); each keeps its own sync folders, state database, and caches.
func (a *App) SwitchAccount(id string) string {
	d, err := config.Resolve()
	if err != nil {
		return err.Error()
	}
	st, err := account.LoadStore(d.AccountsFile())
	if err != nil {
		return err.Error()
	}
	if cur, ok := st.Default(); ok && cur.ID == id {
		return "" // already active
	}
	if err := st.SetDefault(id); err != nil {
		return err.Error()
	}
	a.unmountAllOnDemand()
	a.stopEngine()
	a.start(a.ctx)
	if a.eng == nil {
		return "couldn't start syncing for that account — check its sign-in"
	}
	a.emit("account")
	a.rebuildTrayMenu()
	return ""
}

// AddAccount opens the sign-in window to connect another Nextcloud account.
// The completed login becomes the active account (see account.Complete).
func (a *App) AddAccount() { a.showLogin() }

// RemoveAccount disconnects a NON-active account: its keychain secret, account
// entry, sync-folder setup and local sync state are removed (local files are
// kept). Removing the active account is SignOut's job — it handles the engine
// teardown and next-account handover.
func (a *App) RemoveAccount(id string) string {
	d, err := config.Resolve()
	if err != nil {
		return err.Error()
	}
	st, err := account.LoadStore(d.AccountsFile())
	if err != nil {
		return err.Error()
	}
	if cur, ok := st.Default(); ok && cur.ID == id {
		return "that account is active — use Sign out instead"
	}
	if _, ok := st.Find(id); !ok {
		return "no such account"
	}
	a.stopSecondary(id) // stop its background sync before removing its data
	a.acctMu.Lock()
	delete(a.acctStatus, id)
	a.acctMu.Unlock()
	_ = account.DeleteSecret(id)
	if err := st.Remove(id); err != nil {
		return err.Error()
	}
	a.clearSyncData(d.WithAccount(id), id)
	a.emit("account")
	return ""
}

// SignOut forgets the account (store + keychain), tears down syncing and any
// on-demand mount, closes the signed-in windows, and shows the sign-in window —
// a clean reset to the not-signed-in state. Local files are left untouched.
// (A full process restart is unreliable for a packaged app: the OS job object
// kills any relauncher when we exit, so we reset in place instead.)
// SignOut disconnects the account. With clearData false it's a temporary
// sign-out — the synced-folder setup and local sync database are kept for a
// quick re-login. With clearData true they're wiped too, for a clean reset (and
// so a fresh login can't mistake a since-deleted local folder for user deletions
// and propagate them to the server). UI preferences are kept either way.
func (a *App) SignOut(clearData bool) string {
	if !a.policyNow().AllowSignOut {
		return "Sign out is disabled by your organisation's policy."
	}
	a.unmountAllOnDemand() // disconnect cloud sync roots + stop watchers
	a.stopEngine()
	moreAccounts := false
	if d, err := config.Resolve(); err == nil {
		var accID string
		if st, e := account.LoadStore(d.AccountsFile()); e == nil {
			if acc, ok := st.Default(); ok {
				accID = acc.ID
				_ = account.DeleteSecret(acc.ID)
				_ = st.Remove(acc.ID)
			}
			moreAccounts = len(st.Accounts) > 0
		}
		if clearData {
			a.clearSyncData(d.WithAccount(accID), accID)
		}
	}
	// Another account is configured: hand over to it instead of signing out of
	// the app entirely (its own pairs/state/caches take effect with the engine).
	if moreAccounts {
		a.start(a.ctx)
		if a.eng != nil {
			a.emit("account")
			a.rebuildTrayMenu()
			return ""
		}
	}
	a.setStatus("Signed out")
	// Close any signed-in windows so only the login window remains.
	for _, w := range []**application.WebviewWindow{&a.statusWin, &a.settingsWin, &a.shareWin, &a.versionsWin, &a.logsWin} {
		if *w != nil {
			(*w).Close()
			*w = nil
		}
	}
	a.rebuildTrayMenu()
	a.showLogin()
	return ""
}

// clearSyncData removes this device's sync setup (the pair list) and local sync
// database (the per-account baseline + clone state) plus the on-demand etag
// cache, so a later login starts clean. UI preferences (settings.json) are kept.
func (a *App) clearSyncData(d config.Dirs, accountID string) {
	// d must be account-bound (WithAccount): only the scoped files are removed.
	// Legacy unscoped files need no handling here — engine start migrates them
	// to the active account's scoped names before any UI action can reach this,
	// and deleting them blind could destroy ANOTHER account's unmigrated setup.
	paths := []string{
		d.PairsFile(),
		d.VFSETagsFile(),
		d.VFSFileIDsFile(),
	}
	if accountID != "" {
		db := d.StateDB(accountID)
		paths = append(paths, db, db+"-wal", db+"-shm")
	}
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			slog.Warn("clear sync data", "path", p, "err", err)
		}
	}
	slog.Info("cleared local sync data on sign-out")
}

// onAuthLost is invoked by the engine when the server rejects our credentials:
// stop the failing engine and prompt re-authentication (the account is kept so
// the user just signs in again to the same server).
func (a *App) onAuthLost() {
	application.InvokeAsync(func() {
		a.unmountAllOnDemand()
		a.stopEngine()
		a.setStatus("Sign in again")
		a.rebuildTrayMenu()
		notify.RaiseActionable("Sign in required", brand.Current.Name+" couldn't authenticate — please sign in again.", "action=login",
			[]notify.ToastButton{{Label: "Sign in", Args: "action=login"}})
		a.showLogin()
	})
}

// stopEngine cancels the running engines — primary and secondaries — and
// clears the per-account on-demand stores, so a different account can't
// inherit them.
func (a *App) stopEngine() {
	a.stopSecondaries()
	if a.engCancel != nil {
		a.engCancel()
		a.engCancel = nil
	}
	a.eng = nil
	a.etags = nil
	a.fileids = nil
}

// showLogin opens (or focuses) the sign-in window.
func (a *App) showLogin() {
	if a.loginWin != nil {
		a.loginWin.Show()
		a.loginWin.Focus()
		return
	}
	a.loginWin = a.app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name: "login", Title: brand.Current.Name+" — Sign in", Width: 440, Height: 300, URL: "/#login",
	})
	// Closing the window abandons any in-flight browser login — stop polling so
	// the goroutine doesn't leak (an unapproved flow polls 404 forever).
	a.loginWin.OnWindowEvent(events.Common.WindowClosing, func(*application.WindowEvent) {
		a.cancelLoginPoll()
		a.loginWin = nil
	})
}

// cancelLoginPoll stops any in-flight login-flow poll.
func (a *App) cancelLoginPoll() {
	a.loginMu.Lock()
	cancel := a.loginCancel
	a.loginCancel = nil
	a.loginMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// --- Login (first run) ---

// NeedsLogin reports whether no account is configured yet.
func (a *App) NeedsLogin() bool { return a.eng == nil }

// BeginLogin starts Login Flow v2: it opens the browser and polls in the
// background, emitting "login:done" or "login:error". Returns the login URL (or
// an "error: …" string if it couldn't start).
func (a *App) BeginLogin(server string) string {
	// A managed deployment can pin the server: ignore whatever was typed and
	// sign in to the admin-specified one, so staff can't connect Nimbo to a
	// personal Nextcloud.
	if p := a.policyNow(); p.LockServer && p.ServerURL != "" {
		server = p.ServerURL
	}
	flow, err := account.InitLogin(a.ctx, normalizeServer(server))
	if err != nil {
		return "error: " + err.Error()
	}
	// A fresh attempt supersedes any previous one (the user re-typed / retried);
	// give this poll its own cancellable, time-bounded context. An abandoned
	// Nextcloud login flow polls 404 indefinitely, so without a deadline the
	// window would wait forever — hence the timeout backstop plus cancel-on-close
	// and the frontend Cancel button.
	a.cancelLoginPoll()
	lctx, lcancel := context.WithTimeout(a.ctx, 10*time.Minute)
	a.loginMu.Lock()
	a.loginCancel = lcancel
	a.loginMu.Unlock()

	openURL(flow.LoginURL)
	go func() {
		defer lcancel()
		creds, perr := flow.Poll(lctx)
		if perr != nil {
			switch {
			case errors.Is(lctx.Err(), context.Canceled):
				return // superseded by a newer attempt or the window closed — stay silent
			case errors.Is(lctx.Err(), context.DeadlineExceeded):
				a.app.Event.Emit("login:error", "timed out waiting for browser sign-in — please try again")
			default:
				a.app.Event.Emit("login:error", perr.Error())
			}
			return
		}
		d, cerr := config.Resolve()
		if cerr == nil {
			var st *account.Store
			if st, cerr = account.LoadStore(d.AccountsFile()); cerr == nil {
				_, cerr = account.Complete(st, creds)
			}
		}
		if cerr != nil {
			a.app.Event.Emit("login:error", cerr.Error())
			return
		}
		// Adding an account while one is already syncing: tear the old engine
		// down first — the fresh login is now the default account (Complete
		// sets it) and start() binds to the default.
		if a.eng != nil {
			a.unmountAllOnDemand()
			a.stopEngine()
		}
		a.start(a.ctx)
		a.app.Event.Emit("login:done", nil)
		a.emit("account")
	}()
	return flow.LoginURL
}

// CloseLogin closes the sign-in window.
func (a *App) CloseLogin() {
	if a.loginWin != nil {
		a.loginWin.Close()
		a.loginWin = nil
	}
}

// --- First-run setup (mirrors the official client's final "Add account" step) ---

// SetupInfo seeds the post-login configuration screen.
type SetupInfo struct {
	User            string `json:"user"`
	Server          string `json:"server"`
	DefaultDir      string `json:"defaultDir"`
	AccountBytes    int64  `json:"accountBytes"` // used space on the server ("everything" size)
	FreeBytes       int64  `json:"freeBytes"`    // free space on the chosen local volume
	OnDemandSupport bool   `json:"onDemandSupport"`
}

// GetSetupInfo returns account + storage details for the setup screen.
func (a *App) GetSetupInfo() SetupInfo {
	si := SetupInfo{DefaultDir: a.GetBaseDir(), OnDemandSupport: cfapi.Supported()}
	if a.eng != nil {
		si.User = a.eng.Account.LoginName
		si.Server = a.eng.Account.ServerURL
		if q, err := a.eng.Quota(a.ctx); err == nil {
			si.AccountBytes = q.Used
		}
	}
	si.FreeBytes = int64(diskFree(si.DefaultDir))
	return si
}

// FreeSpace reports the free bytes on the volume holding dir (for live updates
// as the user picks a different folder).
func (a *App) FreeSpace(dir string) int64 { return int64(diskFree(dir)) }

// EnterSetup resizes the sign-in window to fit the configuration screen.
func (a *App) EnterSetup() {
	if a.loginWin != nil {
		a.loginWin.SetSize(560, 640)
		a.loginWin.Center()
	}
}

// CompleteSetup applies the chosen initial-sync option for the whole account
// into localDir, then finishes onboarding. mode is one of:
//
//	"everything" — classic two-way sync of the entire account
//	"choose"     — set the base dir only; the caller opens Sync settings to pick
//	"ondemand"   — virtual files (Cloud Files placeholders, download on open)
//
// Returns "" on success or an error message for the UI.
func (a *App) CompleteSetup(localDir, mode string) string {
	if a.eng == nil {
		return "not signed in"
	}
	if localDir == "" {
		return "choose a local folder"
	}
	switch mode {
	case "everything":
		if msg := a.SetSyncMode("live"); msg != "" {
			return msg
		}
		_ = a.eng.SetBaseDir(localDir)
		return a.AddSyncPair(localDir, "")
	case "choose":
		_ = a.eng.SetBaseDir(localDir)
		return a.SetSyncMode("live")
	case "ondemand":
		if !cfapi.Supported() {
			return "virtual files aren't supported on this system"
		}
		// The account folder becomes the single whole-account virtual-files
		// mount; SetSyncMode("ondemand") mounts it.
		if err := a.eng.SetBaseDir(localDir); err != nil {
			return err.Error()
		}
		return a.SetSyncMode("ondemand")
	default:
		return "unknown setup mode"
	}
}

func normalizeServer(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	return strings.TrimRight(s, "/")
}

// --- Sync settings window ---

// OpenSettings opens (or focuses) the sync-settings window.
func (a *App) OpenSettings() {
	if a.settingsWin != nil {
		a.settingsWin.Show()
		a.settingsWin.Focus()
		return
	}
	a.settingsWin = a.app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name: "settings", Title: brand.Current.Name+" — Settings", Width: 720, Height: 600, URL: "/#settings",
	})
	a.settingsWin.OnWindowEvent(events.Common.WindowClosing, func(*application.WindowEvent) { a.settingsWin = nil })
}

// GetBaseDir / SetBaseDir manage the local root for newly-synced folders.
func (a *App) GetBaseDir() string {
	// When the whole account is synced (a pair with an empty remoteRoot), that
	// pair's local directory IS the account root — prefer it over the stored
	// baseDir, which can go stale if the pair is created or re-pointed without
	// updating it (it would otherwise send "Open sync folder", the Explorer
	// sidebar entry and reveal-in-folder to the wrong place).
	if a.eng != nil {
		if pairs, err := a.eng.Pairs(); err == nil {
			for _, p := range pairs {
				if strings.Trim(p.RemoteRoot, "/") == "" {
					return p.LocalDir
				}
			}
		}
	}
	if d, err := config.Resolve(); err == nil {
		if s, _ := d.LoadSettings(); s.BaseDir != "" {
			return s.BaseDir
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Nextcloud")
}
func (a *App) SetBaseDir(dir string) {
	if a.eng != nil {
		_ = a.eng.SetBaseDir(dir)
	}
}

// healBaseDir repairs a stored baseDir that no longer matches the whole-account
// sync pair (an older setup, or a re-pointed pair, could leave it at the
// default). The empty-remoteRoot pair's local directory is the authoritative
// account root, so persist it. No-op when there's no whole-account pair (e.g.
// "choose what to sync" mode, where baseDir is the parent of several pairs).
func (a *App) healBaseDir() {
	if a.eng == nil {
		return
	}
	pairs, err := a.eng.Pairs()
	if err != nil {
		return
	}
	for _, p := range pairs {
		if strings.Trim(p.RemoteRoot, "/") == "" {
			if cur := a.eng.BaseDir(); cur != p.LocalDir {
				_ = a.eng.SetBaseDir(p.LocalDir)
				slog.Info("healed stale baseDir", "from", cur, "to", p.LocalDir)
			}
			return
		}
	}
}

// BrowseEntry is a remote folder/file for the folder picker.
type BrowseEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"isDir"`
}

// BrowseRemote lists a remote directory (one level), excluding the dir itself.
func (a *App) BrowseRemote(path string) []BrowseEntry {
	if a.eng == nil {
		return nil
	}
	entries, err := a.eng.Browse(a.ctx, path)
	if err != nil {
		slog.Warn("browse failed", "path", path, "err", err)
		return nil
	}
	trimmed := strings.Trim(path, "/")
	out := make([]BrowseEntry, 0, len(entries))
	for _, e := range entries {
		if strings.Trim(e.Path, "/") == trimmed {
			continue
		}
		out = append(out, BrowseEntry{Name: filepath.Base(e.Path), Path: strings.Trim(e.Path, "/"), IsDir: e.IsDir})
	}
	return out
}

// PairDTO describes a configured sync pair.
type PairDTO struct {
	LocalDir   string   `json:"localDir"`
	RemoteRoot string   `json:"remoteRoot"`
	Excludes   []string `json:"excludes"`
}

// GetPairs returns configured sync pairs.
func (a *App) GetPairs() []PairDTO {
	if a.eng == nil {
		return nil
	}
	pairs, _ := a.eng.Pairs()
	out := make([]PairDTO, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, PairDTO{LocalDir: p.LocalDir, RemoteRoot: p.RemoteRoot, Excludes: p.Excludes})
	}
	return out
}

// SyncedRemotes returns the remote paths currently configured as sync pairs.
func (a *App) SyncedRemotes() []string {
	if a.eng == nil {
		return nil
	}
	m := a.eng.SyncedRemotes()
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// AddSyncFolder / RemoveSyncFolder add or remove a synced folder.
func (a *App) AddSyncFolder(remotePath string) {
	if a.eng != nil {
		_ = a.eng.AddSyncFolder(remotePath)
		a.rebuildTrayMenu()
	}
}

// AddSyncPair sets up a sync connection from a remote folder to an explicit
// local directory. Returns "" on success or an error message for the UI.
func (a *App) AddSyncPair(localDir, remotePath string) string {
	if a.eng == nil {
		return "not signed in"
	}
	if err := a.eng.AddSyncPair(localDir, remotePath); err != nil {
		return err.Error()
	}
	// A whole-account pair (empty remote root) defines the account root; keep the
	// stored baseDir in step so the Explorer sidebar and "Open sync folder"
	// resolve to it rather than drifting to the default.
	if strings.Trim(remotePath, "/") == "" {
		_ = a.eng.SetBaseDir(localDir)
	}
	a.rebuildTrayMenu()
	return ""
}

// PickLocalFolder opens a native folder picker and returns the chosen path (or
// "" if cancelled). start seeds the initial directory.
func (a *App) PickLocalFolder(start string) string {
	if a.app == nil {
		return ""
	}
	d := a.app.Dialog.OpenFile().
		CanChooseDirectories(true).
		CanChooseFiles(false).
		CanCreateDirectories(true)
	// Only seed a starting directory that actually exists — pointing the native
	// dialog at a missing path (e.g. a default carried from another machine) can
	// make it fail to open.
	if start != "" {
		if fi, serr := os.Stat(start); serr == nil && fi.IsDir() {
			d = d.SetDirectory(start)
		}
	}
	if a.settingsWin != nil {
		d = d.AttachToWindow(a.settingsWin)
	} else if a.loginWin != nil {
		d = d.AttachToWindow(a.loginWin)
	}
	// The cfd folder picker wraps Windows' COM IFileDialog, which must run on the
	// main STA thread. PickLocalFolder is a bound method invoked on a background
	// goroutine, so marshal the modal dialog onto the main thread — calling it off
	// the main thread makes it silently no-op on some machines (Browse "does
	// nothing"). The other in-process dialogs already go through Invoke*.
	path, err := application.InvokeSyncWithResultAndError(func() (string, error) {
		return d.PromptForSingleSelection()
	})
	if err != nil {
		// A user cancel comes back as an error too — respect it (return no path)
		// rather than popping the PowerShell fallback dialog. Only fall back when
		// the native dialog genuinely failed to open.
		if strings.Contains(strings.ToLower(err.Error()), "cancel") {
			return ""
		}
		slog.Warn("native folder dialog failed; using fallback", "err", err)
		return pickFolderFallback(start)
	}
	return path
}

// pickFolderFallback opens a folder picker in a separate PowerShell process
// (WinForms FolderBrowserDialog, STA). It sidesteps in-process COM/dialog quirks
// that make the native dialog fail to open on some machines.
func pickFolderFallback(start string) string {
	script := "Add-Type -AssemblyName System.Windows.Forms | Out-Null; " +
		"$d = New-Object System.Windows.Forms.FolderBrowserDialog; "
	if start != "" {
		script += "try { $d.SelectedPath = '" + strings.ReplaceAll(start, "'", "''") + "' } catch {}; "
	}
	script += "if ($d.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) { [Console]::Out.Write($d.SelectedPath) }"
	cmd := exec.Command("powershell", "-NoProfile", "-STA", "-Command", script)
	hideConsole(cmd) // suppress the console-window flash; the folder dialog still shows
	out, err := cmd.Output()
	if err != nil {
		slog.Warn("fallback folder dialog failed", "err", err)
		return ""
	}
	return strings.TrimSpace(string(out))
}
func (a *App) RemoveSyncFolder(remotePath string, deleteLocal bool) {
	if a.eng != nil {
		_ = a.eng.RemoveSyncFolder(remotePath, deleteLocal)
		a.rebuildTrayMenu()
	}
}

// AddExclude / RemoveExclude toggle selective-sync excludes within a pair.
func (a *App) AddExclude(localDir, rel string) {
	if a.eng != nil {
		_ = a.eng.AddExclude(localDir, rel)
		a.eng.TriggerSync()
	}
}
func (a *App) RemoveExclude(localDir, rel string) {
	if a.eng != nil {
		_ = a.eng.RemoveExclude(localDir, rel)
		a.eng.TriggerSync()
	}
}

// DeselectFolder stops syncing rel within the pair at localDir. The server copy is
// always kept; deleteLocal also removes the downloaded local copy to free space.
// Returns "" on success or an error string.
func (a *App) DeselectFolder(localDir, rel string, deleteLocal bool) string {
	if a.eng == nil {
		return "not signed in"
	}
	if err := a.eng.DeselectFolder(localDir, rel, deleteLocal); err != nil {
		return err.Error()
	}
	return ""
}

// LimitsDTO is the bandwidth configuration (KiB/s; 0 = unlimited).
type LimitsDTO struct {
	Up   int `json:"up"`
	Down int `json:"down"`
}

func (a *App) GetLimits() LimitsDTO {
	if p := a.policyNow(); p.LockBandwidth {
		return LimitsDTO{Up: p.UploadKBps, Down: p.DownloadKBps}
	}
	if a.eng == nil {
		return LimitsDTO{}
	}
	u, d := a.eng.Limits()
	return LimitsDTO{Up: u, Down: d}
}
func (a *App) SetLimits(up, down int) {
	if a.policyNow().LockBandwidth {
		return // bandwidth is admin-enforced
	}
	if a.eng != nil {
		_ = a.eng.SetLimits(up, down)
	}
}

func (a *App) GetIgnore() []string {
	if a.eng == nil {
		return nil
	}
	p, _ := a.eng.GlobalIgnore()
	return p
}
func (a *App) SetIgnore(patterns []string) {
	if a.eng != nil {
		_ = a.eng.SetGlobalIgnore(patterns)
	}
}

// --- General (autostart) ---

func (a *App) AutostartSupported() bool { return autostart.Supported() }
func (a *App) AutostartEnabled() bool   { ok, _ := autostart.Enabled(); return ok }
func (a *App) SetAutostart(on bool) string {
	exe, err := os.Executable()
	if err != nil {
		return err.Error()
	}
	if on {
		err = autostart.Enable(exe)
	} else {
		err = autostart.Disable()
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

func openURL(href string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", href)
	case "darwin":
		cmd = exec.Command("open", href)
	default:
		cmd = exec.Command("xdg-open", href)
	}
	_ = cmd.Start()
}

// toAgentPairs converts persisted pairs to agent pairs.
func toAgentPairs(pairs []config.SyncPair) []agent.Pair {
	out := make([]agent.Pair, len(pairs))
	for i, p := range pairs {
		out[i] = agent.Pair{LocalDir: p.LocalDir, RemoteRoot: p.RemoteRoot, Excludes: p.Excludes}
	}
	return out
}

func openPath(path string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer", path)
	case "darwin":
		cmd = exec.Command("open", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	_ = cmd.Start()
}

// revealPath opens the file manager with the given file selected (falling back
// to opening its folder where selection isn't supported).
func revealPath(path string) {
	switch runtime.GOOS {
	case "windows":
		_ = exec.Command("explorer", "/select,"+path).Start()
	case "darwin":
		_ = exec.Command("open", "-R", path).Start()
	default:
		openPath(filepath.Dir(path))
	}
}
