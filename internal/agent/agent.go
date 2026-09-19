// Package agent is the reusable sync engine that drives one Nextcloud account:
// it computes and executes reconciliation plans for sync pairs, watches them
// continuously (local fsnotify + notify_push + poll fallback), surfaces app
// notifications, and exposes pause/status controls. Both the CLI and the systray
// GUI build on it so they share identical behaviour.
package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/otherworld/nimbo/internal/account"
	"github.com/otherworld/nimbo/internal/activity"
	"github.com/otherworld/nimbo/internal/cfapi"
	"github.com/otherworld/nimbo/internal/config"
	"github.com/otherworld/nimbo/internal/engine"
	"github.com/otherworld/nimbo/internal/notify"
	"github.com/otherworld/nimbo/internal/policy"
	"github.com/otherworld/nimbo/internal/push"
	"github.com/otherworld/nimbo/internal/state"
	"github.com/otherworld/nimbo/internal/syncguard"
	"github.com/otherworld/nimbo/internal/transfer"
	"github.com/otherworld/nimbo/internal/transport"
	"github.com/otherworld/nimbo/internal/watch"
)

// Pair binds a local directory to a remote folder (files-root-relative; "" is
// the whole files root). Excludes are pair-specific ignore patterns.
type Pair struct {
	LocalDir   string
	RemoteRoot string
	Excludes   []string
}

// Engine holds the shared per-account resources and runtime state.
type Engine struct {
	Account  account.Account
	dirs     config.Dirs
	client   *transport.Client
	secret   string
	caps     *transport.Capabilities
	notifier *notify.Notifier
	recorder *activity.Recorder
	// forbidden (the server's name rules + the user allow-list) and escaper (opt-in
	// forbidden-name escaping) are held atomically so an allow-list or escape-list
	// change can swap them live between syncs, instead of only on next launch.
	forbidden atomic.Pointer[engine.Forbidden]
	escaper   atomic.Pointer[engine.Escaper]
	// guard is the account's damage-guard state (which folders are frozen),
	// reloaded at the top of every sync pass (see ensurePair). A nil pointer
	// means the state could not be READ — distinct from an empty set, which
	// means "nothing is frozen". See guardStateUnavailable.
	guard atomic.Pointer[config.GuardStates]

	mu            sync.Mutex
	paused        bool          // indefinite manual pause
	pauseUntil    time.Time     // timed pause expiry (zero = none)
	schedule      PauseSchedule // quiet-hours auto-pause window
	onStatus      func(string)
	onPauseChange func() // notified when the effective pause state changes

	// Diagnostics (surfaced in the app's health panel).
	diagMu     sync.Mutex
	pushUp     bool      // notify_push WebSocket currently connected
	pushSince  time.Time // when it last connected (uptime)
	lastStatus string    // most recent status string
	lastSyncAt time.Time // when the engine last reached "Up to date"

	// Dynamic watcher state (set once Run starts). watchers/triggers are keyed by
	// pair key so folders can be added/removed live via ReloadPairs.
	runCtx       context.Context
	onSync       func(Pair, transfer.Stats)
	pollInterval time.Duration
	watchMu      sync.Mutex
	watchers     map[string]context.CancelFunc
	triggers     map[string]chan struct{}
	triggersFull map[string]chan struct{} // key -> force-a-full-local-pass trigger (name-rule changes)
	watchDone    map[string]chan struct{} // key -> closed when the watcher goroutine exits (for a synchronous, drained stop)

	// Per-pair sync health (see pairhealth.go). Lazily created so a zero-value
	// Engine — constructed before Run — works without setup.
	pairHealthOnce  sync.Once
	pairHealthState *pairHealthState

	// moveExcl enforces move/sync mutual exclusion. A sync pass holds it for
	// reading (many may run concurrently); a "Move sync folder" holds it for
	// writing (exclusive). Both use the non-blocking Try variants: a move that
	// finds a sync running fails fast, and a sync that finds a move running
	// skips — so a move and a sync can never overlap. This makes the original
	// data-loss bug (a mid-move folder read as mass server deletions) structurally
	// impossible, instead of something we only guard against after the fact.
	moveExcl sync.RWMutex

	blockedMu   sync.Mutex
	blocked     map[string][]engine.Blocked // key = pair LocalDir
	blockedSubs []chan struct{}

	lockedMu   sync.Mutex
	locked     map[string][]LockedFile // key = pair LocalDir — files OTHER users hold locked
	lockedSubs []chan struct{}
	lockToast  map[string]time.Time // path+owner -> last toast, to survive a flapping lock

	// statusIcons registers live sync folders as cloud sync roots so Explorer
	// draws native sync-state icons on them — the only route that works for a
	// packaged build (see statusroot_windows.go).
	statusIcons *statusRoots

	// lockWarn makes OTHER people's locks visible locally: the deny-write handle
	// plus the synthesised name carrier. Separate from lockMgr, which owns the
	// locks we take ourselves.
	lockWarn *lockWarner

	// lockMgr owns the locks WE take (as opposed to `locked`, which is what other
	// people hold). Separate because their lifetimes are entirely different: ours
	// must be released or they last forever.
	lockMgr *lockMgr

	// failLog dedupes per-action sync failures so a permanently-rejected item
	// (e.g. something dropped into the app-managed .Collectives folder) is logged
	// once with a human-readable reason instead of spamming every sync pass.
	failMu   sync.Mutex
	lastFail map[string]string // "<kind>\x00<path>" -> last human reason logged

	damagedMu sync.Mutex
	damaged   map[string]string // "<pairKey>\x00<path>" -> server etag of a copy that failed its checksum

	policy       transfer.ConflictPolicy
	conflictMu   sync.Mutex
	conflicts    map[string][]ConflictItem // key = pair LocalDir
	conflictSubs []chan struct{}

	detachedMu   sync.Mutex
	detachedSubs []chan struct{} // notified when the parked-folder list changes (see detach.go)

	inflightMu       sync.Mutex
	inflight         map[string]bool // absolute paths currently being transferred
	onOverlayRefresh func(string)    // notified when a path's sync state changes

	overlayRootsMu sync.RWMutex
	overlayRoots   []string // extra synced dirs with no live pair (on-demand mounts)

	sharedMu          sync.RWMutex
	sharedRemote      map[string]bool // files-root-relative paths carrying a share
	receivedShares    map[string]bool // the received-from-others subset, for arrival toasts
	sharesPrimed      bool            // first refresh done — only later arrivals toast
	sharesRefreshedAt time.Time       // when refreshSharesSoon last ran (its throttle)

	ensuredMu    sync.Mutex
	ensuredRoots map[string]bool // pair remote roots already MKCOLed this run (#599)

	// scanLastEmit throttles the scan heartbeat (see scanstatus.go): the scan
	// callbacks fire per directory/entry from worker goroutines, far faster than
	// the UI needs or can render.
	scanMu       sync.Mutex
	scanStart    time.Time // when the current scan pass began (quiet-period gate)
	scanLastEmit time.Time

	progMu      sync.Mutex
	prog        SyncProgress
	progRuns    int           // in-flight SyncOnce runs contributing to progress
	progBytes   atomic.Int64  // cumulative bytes transferred in the current burst
	progStop    chan struct{} // stops the speed sampler
	progStartAt time.Time     // burst start, for a stable average-rate ETA
	onProgress  func(SyncProgress)

	onToast      func(title, message, link string) // desktop toasts (GUI sets this)
	encMu        sync.Mutex
	encSeen      map[string]bool // E2EE folders already notified (once per engine)
	toastMu      sync.Mutex
	lastErrToast time.Time

	onAuthLost    func() // called when the server rejects our credentials
	authLostFired bool

	onFilesChanged func() // notify_push reported a server-side file change (on-demand reconcile)

	storeMu    sync.Mutex   // guards lazy creation of store
	store      *state.Store // resident state DB handle + baseline cache, opened on first use
	storeFinal bool         // Run has exited: refuse lazy reopens (nothing would close them)

	cpMu    sync.Mutex      // guards cpClean
	cpClean map[string]bool // pair_key -> checkpoint rows known deleted; missing = assume dirty

	stateResetToast sync.Once       // the state-reset warning toasts once per engine run
	histMu          sync.Mutex      // serializes sync-history marker writes
	histMarked      map[string]bool // pair_key -> marker known present (this run)
}

// SyncProgress is a live snapshot of an in-progress sync, for the UI.
type SyncProgress struct {
	Active      bool   `json:"active"`
	Current     string `json:"current"`     // file currently transferring (basename)
	Done        int    `json:"done"`        // transfers completed this burst
	Total       int    `json:"total"`       // transfers planned this burst (grows as a clone enumerates)
	Speed       int64  `json:"speed"`       // bytes/sec, smoothed (EMA) — for the live readout
	AvgSpeed    int64  `json:"avgSpeed"`    // bytes/sec, cumulative average since burst start — for a stable ETA
	DoneBytes   int64  `json:"doneBytes"`   // bytes transferred this burst
	TotalBytes  int64  `json:"totalBytes"`  // bytes planned this burst (for % and ETA)
	Enumerating bool   `json:"enumerating"` // still discovering work — total not yet final, so show indeterminate
}

// ConflictItem is a deferred conflict awaiting a user choice (PolicyAsk).
type ConflictItem struct {
	LocalDir     string
	RemoteRoot   string
	Path         string // pair-relative
	Kind         string // edited | deleted-locally | deleted-remotely | type
	LocalExists  bool
	RemoteExists bool
	LocalSize    int64 // per-side metadata captured at detection (no live round trip)
	LocalMTime   time.Time
	RemoteSize   int64
	RemoteMTime  time.Time
}

// NewEngine builds an engine for the default account, loading its app password
// from the keychain and discovering server capabilities (which determines
// whether real-time push is available).
func NewEngine(ctx context.Context) (*Engine, error) { return NewEngineFor(ctx, "") }

// NewEngineFor builds an engine for a specific configured account (or the
// default when accountID is empty) — the basis for several accounts syncing
// side by side, each engine bound to its own pairs, state DB, and caches.
func NewEngineFor(ctx context.Context, accountID string) (*Engine, error) {
	d, err := config.Resolve()
	if err != nil {
		return nil, err
	}
	st, err := account.LoadStore(d.AccountsFile())
	if err != nil {
		return nil, err
	}
	var (
		acc account.Account
		ok  bool
	)
	if accountID == "" {
		acc, ok = st.Default()
	} else {
		acc, ok = st.Find(accountID)
	}
	if !ok {
		return nil, fmt.Errorf("no account configured — run: nimbo login <server-url>")
	}
	// Scope per-account files (sync pairs, on-demand etags) to this account so
	// each configured account keeps its own folder setup; adopt the legacy
	// single-account files on first run after the multi-account change.
	d = d.WithAccount(acc.ID)
	d.MigratePairs()
	secret, err := account.LoadSecret(acc.ID)
	if err != nil {
		return nil, err
	}
	client := transport.New(acc.ServerURL, acc.LoginName, secret)
	if s, serr := d.LoadSettings(); serr == nil {
		client.SetLimits(s.UploadKBps, s.DownloadKBps)
	}
	// The local network route, if set up. Probing BEFORE FetchCapabilities is
	// deliberate: a start with the internet down but the LAN up still works.
	// Away from the LAN this costs at most the 3 s dial timeout.
	if lr := acc.Local; lr != nil {
		if err := client.SetLocalRoute(lr.Address, lr.Pin, lr.RootID); err != nil {
			slog.Warn("local network route ignored", "err", err)
		} else if perr := client.ProbeLocal(ctx); perr == nil {
			slog.Info(routeSwitchMessage(transport.RouteLocal, "", client.LocalAddress()))
		} else {
			_, reason := client.Route()
			slog.Info(routeSwitchMessage(transport.RoutePublic, reason, lr.Address), "err", perr)
		}
	}
	caps, err := client.FetchCapabilities(ctx)
	if err != nil {
		return nil, err
	}
	names := append(append([]string{}, caps.Files.ForbiddenFilenames...), caps.Files.BlacklistedFiles...)
	var allowed, escapeExts []string
	if s, serr := d.LoadSettings(); serr == nil {
		allowed = s.AllowedFilenames
		escapeExts = s.EscapeExtensions
	}
	forbidden := engine.NewForbidden(names, caps.Files.ForbiddenBasenames, caps.Files.ForbiddenCharacters, caps.Files.ForbiddenExtensions, allowed)
	// Opt-in escaping of server-forbidden names (e.g. .htaccess -> .htaccess.nimboesc).
	// Inactive with no opted-in extensions, so this is a no-op by default. A managed
	// deployment can force it off (the host's block is intentional there).
	if policy.Load().DisableNameEscaping {
		escapeExts = nil
	}
	escaper := engine.NewEscaper(forbidden, escapeExts, "")
	slog.Debug("forbidden rules from server",
		"names", len(caps.Files.ForbiddenFilenames), "blacklisted", len(caps.Files.BlacklistedFiles),
		"basenames", len(caps.Files.ForbiddenBasenames), "exts", len(caps.Files.ForbiddenExtensions),
		"chars", len(caps.Files.ForbiddenCharacters))

	eng := &Engine{
		Account:   acc,
		dirs:      d,
		client:    client,
		secret:    secret,
		caps:      caps,
		notifier:  notify.New(client, acc.ID),
		recorder:  activity.New(),
		blocked:   make(map[string][]engine.Blocked),
		conflicts: make(map[string][]ConflictItem),
		locked:    make(map[string][]LockedFile),
		lockToast: make(map[string]time.Time),
	}
	eng.lockMgr = newLockMgr(client, d, acc.LoginName)
	eng.lockWarn = newLockWarner(d, func() string { return acc.LoginName })
	eng.statusIcons = newStatusRoots()
	eng.forbidden.Store(forbidden)
	eng.escaper.Store(escaper)
	// Load the backup set before the engine is handed out: the zero value reads
	// as "unreadable", which suspends every pair. An unreadable file here leaves
	// it that way deliberately — the engine still builds, and every sync refuses
	// with a message saying why, rather than silently reverting one-way folders
	// to two-way and uploading the local mirror.
	_ = eng.reloadGuardState()
	return eng, nil
}

// SetConflictPolicy sets how conflicts are handled. The GUI uses PolicyAsk to
// defer to the user; the CLI keeps the default PolicyAuto.
func (e *Engine) SetConflictPolicy(p transfer.ConflictPolicy) { e.policy = p }

// Client exposes the underlying transport client (for CLI commands that need it).
func (e *Engine) Client() *transport.Client { return e.client }

// Recorder exposes the activity/error log for UIs.
func (e *Engine) Recorder() *activity.Recorder { return e.recorder }

// Notifier exposes the notification view (List/Subscribe/Count) for UIs.
func (e *Engine) Notifier() *notify.Notifier { return e.notifier }

// DismissNotification removes a notification and refreshes the list.
func (e *Engine) DismissNotification(ctx context.Context, id int) error {
	if err := e.client.DismissNotification(ctx, id); err != nil {
		return err
	}
	return e.notifier.Refresh(ctx)
}

// DismissAllNotifications clears every notification for the account and refreshes
// the list.
func (e *Engine) DismissAllNotifications(ctx context.Context) error {
	if err := e.client.DismissAllNotifications(ctx); err != nil {
		return err
	}
	return e.notifier.Refresh(ctx)
}

// DoNotificationAction runs a notification action (Accept/Decline/…) and
// refreshes the list.
func (e *Engine) DoNotificationAction(ctx context.Context, a transport.NotificationAction) error {
	if err := e.client.ExecuteAction(ctx, a); err != nil {
		return err
	}
	return e.notifier.Refresh(ctx)
}

// PushAvailable reports whether the server offers notify_push.
func (e *Engine) PushAvailable() bool { return e.caps.NotifyPush != nil }

// LockingAvailable reports whether the server has the files_lock app, which the
// whole file-locking feature is gated on. Capabilities are fetched once at
// sign-in, so an admin installing the app while Nimbo runs is not noticed until
// restart — the same caveat that already applies to notify_push.
func (e *Engine) LockingAvailable() bool {
	return e.caps != nil && e.caps.Files.Locking != ""
}

// ThemeColor returns the user's Nextcloud primary theme colour (hex), or "" if
// the server doesn't advertise one.
func (e *Engine) ThemeColor() string {
	if e.caps != nil {
		return e.caps.Theming.Color
	}
	return ""
}

// ThemeAppearance reports the user's Nextcloud appearance ("dark", "light", or
// "default" when they follow the system). Unlike ThemeColor this needs a live
// request, as the server advertises no cached capability for it.
func (e *Engine) ThemeAppearance(ctx context.Context) (string, error) {
	return e.client.ThemeAppearance(ctx)
}

// ServerURL returns the account's server base URL.
func (e *Engine) ServerURL() string { return e.Account.ServerURL }

// Apps returns the user's Nextcloud app menu.
func (e *Engine) Apps(ctx context.Context) ([]transport.App, error) {
	return e.client.NavigationApps(ctx)
}

// Quota returns the user's storage usage.
func (e *Engine) Quota(ctx context.Context) (transport.QuotaInfo, error) {
	return e.client.UserQuota(ctx)
}

// Preview returns a server-rendered thumbnail for a file id, at most px pixels
// per side. Images, PDFs and office documents all answer; anything else errors.
func (e *Engine) Preview(ctx context.Context, fileID string, px int) ([]byte, error) {
	return e.client.Preview(ctx, fileID, px)
}

// --- Trashbin & file versions (server features surfaced in the GUI) ---

// Trash returns the items in the Nextcloud trashbin.
func (e *Engine) Trash(ctx context.Context) ([]transport.TrashItem, error) {
	return e.client.ListTrash(ctx)
}

// RestoreTrash restores a trashed item and triggers a sync to pull it down.
func (e *Engine) RestoreTrash(ctx context.Context, href string) error {
	if err := e.client.RestoreTrash(ctx, href); err != nil {
		return err
	}
	e.TriggerSync()
	return nil
}

// DeleteTrash permanently removes a trashed item.
func (e *Engine) DeleteTrash(ctx context.Context, href string) error {
	return e.client.DeleteTrash(ctx, href)
}

// Versions lists stored versions of a file by its oc:fileid.
func (e *Engine) Versions(ctx context.Context, fileID string) ([]transport.FileVersion, error) {
	return e.client.ListVersions(ctx, fileID)
}

// RestoreVersion makes a stored version current and triggers a sync.
func (e *Engine) RestoreVersion(ctx context.Context, href string) error {
	if err := e.client.RestoreVersion(ctx, href); err != nil {
		return err
	}
	e.TriggerSync()
	return nil
}

// StatRemote returns server metadata (incl. oc:fileid) for a files-root-relative
// path — used to find the fileid for version history.
func (e *Engine) StatRemote(ctx context.Context, remotePath string) (transport.Entry, bool, error) {
	return e.client.Stat(ctx, remotePath)
}

// OpenRange streams up to length bytes starting at offset in a remote file —
// one bounded GET, however large the range, rather than an open-ended stream
// that gets abandoned after each caller-sized chunk. A server that sends
// fewer bytes than requested ends the reader early (EOF); it is up to the
// caller to judge whether that's acceptable. Callers must Close the reader.
func (e *Engine) OpenRange(ctx context.Context, remotePath string, offset, length int64) (io.ReadCloser, error) {
	body, _, err := e.client.GetRange(ctx, remotePath, offset, length)
	if err != nil {
		return nil, err
	}
	if length > 0 {
		return struct {
			io.Reader
			io.Closer
		}{io.LimitReader(body, length), body}, nil
	}
	return body, nil
}

// DownloadRange returns up to length bytes of a remote file starting at offset
// (used to hydrate on-demand placeholders).
func (e *Engine) DownloadRange(ctx context.Context, remotePath string, offset, length int64) ([]byte, error) {
	body, err := e.OpenRange(ctx, remotePath, offset, length)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	buf := make([]byte, length)
	n, err := io.ReadFull(body, buf)
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return buf[:n], nil
	}
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// DownloadTo streams a remote file to localPath, creating its parent
// directories. Unlike DownloadRange it never holds the file in memory, so it is
// safe for the arbitrarily large files a file-manager UI will ask for.
//
// It writes to a temp file in the destination directory and renames on success,
// so a failed or cancelled download leaves nothing behind: callers cache by file
// id, and a truncated leftover would otherwise be served forever as the real file.
func (e *Engine) DownloadTo(ctx context.Context, remotePath, localPath string) error {
	dir := filepath.Dir(localPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	body, _, err := e.client.Get(ctx, remotePath)
	if err != nil {
		return err
	}
	defer body.Close()

	tmp, err := os.CreateTemp(dir, ".download-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()        // second close is harmless
		os.Remove(tmpName) // no-op once the rename below has succeeded
	}()

	if _, err := io.Copy(tmp, body); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, localPath)
}

// Upload pushes localPath's content to the files-root-relative remotePath,
// creating parent collections as needed (chunked for large files). Used by the
// on-demand write-back watcher; it does NOT touch the diff-engine baseline.
//
// It joins the standard progress burst, so the flyout shows the file name,
// bytes, speed and ETA exactly like a classic sync — before this, a multi-hour
// on-demand upload ran with the UI claiming "Up to date" throughout (issue #1:
// "does not show any sign in the app that it is doing so").
// The returned ETag is the uploaded revision's own (authoritative — a
// baseline recorded from a later Stat could adopt a concurrent writer's
// revision as "already synced"); it may be empty on servers that omit it.
func (e *Engine) Upload(ctx context.Context, localPath, remotePath string) (string, error) {
	remotePath = strings.Trim(remotePath, "/")
	if i := strings.LastIndex(remotePath, "/"); i > 0 {
		if err := e.client.EnsureCollection(ctx, remotePath[:i]); err != nil {
			return "", err
		}
	}
	var size int64
	if fi, err := os.Stat(localPath); err == nil {
		size = fi.Size()
	}
	e.progStart(1, size)
	defer e.progEnd()
	e.progCurrent(filepath.Base(localPath))
	res, err := transfer.UploadProgress(ctx, e.client, localPath, remotePath,
		func(n int64) { e.progBytes.Add(n) })
	if err == nil {
		e.progComplete()
	}
	return res.ETag, err
}

// MkdirRemote creates remotePath (and any missing parents) on the server.
func (e *Engine) MkdirRemote(ctx context.Context, remotePath string) error {
	return e.client.EnsureCollection(ctx, strings.Trim(remotePath, "/"))
}

// DeleteRemote removes the file or directory at remotePath on the server.
func (e *Engine) DeleteRemote(ctx context.Context, remotePath string) error {
	return e.client.Delete(ctx, strings.Trim(remotePath, "/"))
}

// MoveRemote moves/renames src to dst on the server (creating dst's parents).
func (e *Engine) MoveRemote(ctx context.Context, src, dst string) error {
	dst = strings.Trim(dst, "/")
	if i := strings.LastIndex(dst, "/"); i > 0 {
		if err := e.client.EnsureCollection(ctx, dst[:i]); err != nil {
			return err
		}
	}
	return e.client.Move(ctx, strings.Trim(src, "/"), dst)
}

// SearchFiles queries the server's unified search for files matching term.
func (e *Engine) SearchFiles(ctx context.Context, term string, limit int) ([]transport.SearchResult, error) {
	return e.client.SearchFiles(ctx, term, limit)
}

// PinnedApps returns the IDs of apps pinned to the flyout.
func (e *Engine) PinnedApps() []string {
	s, _ := e.dirs.LoadSettings()
	return s.PinnedApps
}

// PinApp pins an app by ID (no-op if already pinned).
func (e *Engine) PinApp(id string) error {
	return e.dirs.UpdateSettings(func(s *config.Settings) {
		for _, p := range s.PinnedApps {
			if p == id {
				return
			}
		}
		s.PinnedApps = append(s.PinnedApps, id)
	})
}

// UnpinApp removes an app from the pinned list.
func (e *Engine) UnpinApp(id string) error {
	return e.dirs.UpdateSettings(func(s *config.Settings) {
		out := s.PinnedApps[:0]
		for _, p := range s.PinnedApps {
			if p != id {
				out = append(out, p)
			}
		}
		s.PinnedApps = out
	})
}

// SearchByName finds files and folders by name across the whole account,
// returning them as ordinary entries with real paths (unlike SearchFiles,
// whose unified-search hits carry only display text and a web URL).
func (e *Engine) SearchByName(ctx context.Context, term string, limit int) ([]transport.Entry, error) {
	return e.client.SearchByName(ctx, term, limit)
}

// Favorites returns the user's favourited files and folders.
func (e *Engine) Favorites(ctx context.Context) ([]transport.Entry, error) {
	return e.client.Favorites(ctx)
}

// Shares returns every share this account takes part in, split into the ones
// the user created and the ones other people shared with them.
func (e *Engine) Shares(ctx context.Context) (own, received []transport.Share, err error) {
	return e.client.ListAllShares(ctx)
}

// SetFavorite stars or unstars a file or folder.
func (e *Engine) SetFavorite(ctx context.Context, remotePath string, fav bool) error {
	return e.client.SetFavorite(ctx, remotePath, fav)
}

// UserStatus returns the user's Nextcloud presence/status.
func (e *Engine) UserStatus(ctx context.Context) (transport.UserStatusInfo, error) {
	return e.client.UserStatus(ctx)
}

// SetUserStatusType sets the presence (online/away/dnd/invisible).
func (e *Engine) SetUserStatusType(ctx context.Context, t string) error {
	return e.client.SetUserStatusType(ctx, t)
}

// SetUserStatusMessage sets a custom status message + optional emoji.
func (e *Engine) SetUserStatusMessage(ctx context.Context, msg, icon string) error {
	return e.client.SetUserStatusMessage(ctx, msg, icon)
}

// ClearUserStatusMessage clears the custom status message.
func (e *Engine) ClearUserStatusMessage(ctx context.Context) error {
	return e.client.ClearUserStatusMessage(ctx)
}

// Pairs returns the configured sync pairs.
func (e *Engine) Pairs() ([]config.SyncPair, error) {
	return e.dirs.LoadPairs()
}

// BlockedFile is a file that can't sync because the server forbids its name.
type BlockedFile struct {
	LocalDir string // the sync pair's local root
	Path     string // pair-relative path
	Abs      string // absolute local path
	Reason   string
	IsDir    bool
}

// BlockedFiles returns all currently-blocked files across pairs.
func (e *Engine) BlockedFiles() []BlockedFile {
	e.blockedMu.Lock()
	defer e.blockedMu.Unlock()
	var out []BlockedFile
	for dir, list := range e.blocked {
		for _, b := range list {
			out = append(out, BlockedFile{
				LocalDir: dir,
				Path:     b.Path,
				Abs:      filepath.Join(dir, filepath.FromSlash(b.Path)),
				Reason:   b.Reason,
				IsDir:    b.IsDir,
			})
		}
	}
	return out
}

// LockedFile is a file another user currently holds a files_lock lock on.
type LockedFile struct {
	LocalDir     string // the sync pair's local root
	Path         string // pair-relative path
	Abs          string // absolute local path
	Owner        string // login name of the lock holder
	OwnerDisplay string // display name, when the server gave one
	AppName      string // for an app lock, the editor holding it (e.g. "Text"); empty otherwise
	OwnerType    transport.LockOwnerType
	Since        time.Time
}

// Who names the lock holder as a human would. An APP lock has no person behind
// it — nc:lock-owner is absent and nc:lock-owner-displayname carries the app's
// name — so callers must check AppName first; Who deliberately never returns an
// app name as if it were a colleague.
func (f LockedFile) Who() string {
	if f.AppName != "" {
		return "Someone else"
	}
	if f.OwnerDisplay != "" {
		return f.OwnerDisplay
	}
	if f.Owner != "" {
		return f.Owner
	}
	return "Someone else"
}

// Summary is the one-line description shown in a toast and in the status list.
func (f LockedFile) Summary() string {
	name := filepath.Base(f.Path)
	if f.AppName != "" {
		// e.g. "New notes.md is open in Nextcloud Text" — the server tells us the
		// app but not the person, so do not invent one.
		return name + " is open in Nextcloud " + f.AppName
	}
	return f.Who() + " has " + name + " open"
}

// LockedFiles returns every file other users have locked, across pairs.
func (e *Engine) LockedFiles() []LockedFile {
	e.lockedMu.Lock()
	defer e.lockedMu.Unlock()
	var out []LockedFile
	for dir, list := range e.locked {
		for _, f := range list {
			f.LocalDir = dir
			f.Abs = filepath.Join(dir, filepath.FromSlash(f.Path))
			out = append(out, f)
		}
	}
	return out
}

// SubscribeLocked returns a channel signalled when the locked set changes.
func (e *Engine) SubscribeLocked() <-chan struct{} {
	ch := make(chan struct{}, 1)
	e.lockedMu.Lock()
	e.lockedSubs = append(e.lockedSubs, ch)
	e.lockedMu.Unlock()
	return ch
}

// EnableStatusIcons registers a live sync folder as a status-only cloud sync
// root, which gives it Explorer's Status column; the per-item icons come from
// NCOverlays.dll's CustomStateHandler asking FileStatus over the status pipe.
// Applied to every live pair automatically — there is no user toggle.
func (e *Engine) EnableStatusIcons(dir, displayName, iconPath string) error {
	return e.statusIcons.enable(dir, displayName, iconPath)
}

// DisableStatusIcons unregisters the root — for a removed pair, or a folder
// about to be claimed by an on-demand mount (nested sync roots are refused).
func (e *Engine) DisableStatusIcons(dir string) { e.statusIcons.disable(dir) }

// TakeLock locks a remote path on the server so other people are told the file
// is in use. Nimbo owns the lock's whole lifetime from here — see lockMgr.
func (e *Engine) TakeLock(ctx context.Context, remotePath string) error {
	if !e.LockingAvailable() {
		return fmt.Errorf("this server doesn't have the Files Lock app")
	}
	return e.lockMgr.take(ctx, remotePath)
}

// ReleaseLock drops a lock we took. A path we do not hold is a no-op, never an
// UNLOCK — releasing somebody else's lock returns 423 and is not ours to do.
func (e *Engine) ReleaseLock(ctx context.Context, remotePath string) error {
	return e.lockMgr.release(ctx, remotePath)
}

// ReleaseAllLocks drops every lock this account holds and reports how many went.
// This is the user's escape hatch as well as the shutdown path: the server never
// expires a lock, so without it a stuck one would need admin intervention.
func (e *Engine) ReleaseAllLocks(ctx context.Context) (int, error) {
	return e.lockMgr.releaseAll(ctx)
}

// HeldLocks lists the locks Nimbo currently holds for this account.
func (e *Engine) HeldLocks() []config.HeldLock { return e.lockMgr.list() }

// lockHeartbeat is how often we re-assert the locks we hold. Re-locking is the
// only refresh mechanism files_lock offers, and it matters on any server whose
// admin has configured lock_timeout — without it their locks would expire under
// a user who still has the file open.
const lockHeartbeat = 5 * time.Minute

// runLockLifetime sweeps locks a previous run stranded, then keeps ours alive
// until the context ends, releasing everything on the way out.
//
// It deliberately does NOT consult e.Paused() and never takes moveExcl: pausing
// syncing must not stop us releasing a lock, and PauseSchedule auto-pauses
// nightly — driving this from a sync path would strand every lock until morning.
func (e *Engine) runLockLifetime(ctx context.Context) {
	if !e.LockingAvailable() {
		return
	}
	if n, err := e.lockMgr.sweep(ctx); n > 0 || err != nil {
		slog.Info("swept locks left by a previous run", "released", n, "err", err)
	}
	if e.lockWarn != nil {
		if n := e.lockWarn.sweep(); n > 0 {
			slog.Info("removed lock warnings left by a previous run", "files", n)
		}
	}
	t := time.NewTicker(lockHeartbeat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// Best effort on the way out, on a fresh context: ctx is already dead,
			// and a lock we fail to release here lasts forever.
			if e.lockWarn != nil {
				e.lockWarn.closeAll() // never leave a user unable to edit their own file
			}
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
			if n, err := e.lockMgr.releaseAll(rctx); n > 0 || err != nil {
				slog.Info("released locks on shutdown", "released", n, "err", err)
			}
			cancel()
			return
		case <-t.C:
			e.lockMgr.heartbeat(ctx)
		}
	}
}

// heldStatus is the flyout line for a pass that finished with nothing to do.
// Held uploads are the exception to "Up to date".
func heldStatus(held []string) string {
	switch len(held) {
	case 0:
		return "Up to date"
	case 1:
		return "Waiting — " + filepath.Base(held[0]) + " is in use by someone else"
	default:
		return fmt.Sprintf("Waiting — %d files are in use by someone else", len(held))
	}
}

// lockScan reads a pass's remote map and returns the paths whose lock state it
// can vouch for, plus the ones another user holds. Split out of applyPlan so the
// examined/locked distinction is unit-testable.
func lockScan(remote map[string]engine.RemoteState, login, localDir string) (map[string]bool, []LockedFile) {
	examined := make(map[string]bool, len(remote))
	var locked []LockedFile
	for rel, r := range remote {
		if r.IsDir || !r.LockKnown {
			continue // a pruned subtree was replayed from the baseline: it did not look
		}
		examined[rel] = true
		if !r.Lock.HeldByOther(login) {
			continue
		}
		locked = append(locked, LockedFile{
			Path: rel, LocalDir: localDir,
			Owner: r.Lock.Owner, OwnerDisplay: r.Lock.OwnerDisplay,
			AppName:   r.Lock.AppName(),
			OwnerType: r.Lock.OwnerType, Since: r.Lock.Since,
		})
	}
	return examined, locked
}

// lockToastWindow is how long a given lock stays "already announced". A lock can
// legitimately vanish and reappear between passes — its subtree gets pruned by
// its ETag and replayed from the baseline, which carries no lock state — and
// without this window that flapping would toast the user repeatedly.
const lockToastWindow = time.Hour

// reconcileLocked updates a pair's locked set from one sync pass.
//
// examined is the set of paths this pass had authoritative lock knowledge for;
// l is the subset another user holds. Entries for examined paths are replaced
// wholesale (so a RELEASED lock disappears), and entries for paths this pass
// never looked at are left exactly as they were.
//
// It deliberately does NOT copy the blocked-files merge-only rule. Merge-only is
// right for a blocked file — a forbidden name stays forbidden until the user
// renames it — but a lock is transient, and merge-only meant a released lock was
// only ever cleared by the hourly full reconcile. Found on the 2026-08-09 beta:
// every routine pass is a delta, so a lock could sit in the UI for an hour after
// the other user closed the file.
func (e *Engine) reconcileLocked(localDir string, examined map[string]bool, l []LockedFile) {
	e.lockedMu.Lock()
	if e.locked == nil {
		e.locked = make(map[string][]LockedFile) // engines built outside NewEngineFor (tests)
	}
	old := e.locked[localDir]
	next := make([]LockedFile, 0, len(old)+len(l))
	for _, x := range old {
		if !examined[x.Path] {
			next = append(next, x) // not looked at this pass — leave it alone
		}
	}
	next = append(next, l...)
	if len(next) == 0 {
		delete(e.locked, localDir)
	} else {
		e.locked[localDir] = next
	}
	subs := append([]chan struct{}(nil), e.lockedSubs...)

	// Work out the transitions while we still hold the mutex, so the toast
	// bookkeeping cannot race another pass.
	was := make(map[string]bool, len(old))
	for _, x := range old {
		was[x.Path+"\x00"+x.Owner] = true
	}
	held := make(map[string]bool, len(l))
	for _, nl := range l {
		held[nl.Path] = true
	}

	// Every transition is logged, whether or not it toasts. Diagnosing a lock
	// complaint otherwise means probing the server by hand — the log said nothing
	// either way, which cost real time on the 2026-08-09 beta.
	var added, removed []LockedFile
	for _, nl := range l {
		if !was[nl.Path+"\x00"+nl.Owner] {
			added = append(added, nl)
		}
	}
	for _, x := range old {
		if examined[x.Path] && !held[x.Path] {
			removed = append(removed, x)
		}
	}

	// Toasting is narrower than logging: it also honours the re-notify window, so
	// a lock that flaps across passes does not nag.
	var fresh []LockedFile
	if e.onToast != nil {
		now := time.Now()
		for _, nl := range added {
			key := nl.Path + "\x00" + nl.Owner
			if last, ok := e.lockToast[key]; ok && now.Sub(last) < lockToastWindow {
				continue
			}
			if e.lockToast == nil {
				e.lockToast = make(map[string]time.Time)
			}
			e.lockToast[key] = now
			fresh = append(fresh, nl)
		}
	}
	e.lockedMu.Unlock()
	notifyAll(subs)

	for _, f := range added {
		slog.Info("file locked by someone else", "path", f.Path,
			"by", f.Who(), "app", f.AppName, "type", int(f.OwnerType))
	}
	for _, f := range removed {
		slog.Info("lock released", "path", f.Path, "by", f.Who(), "app", f.AppName)
	}
	if len(removed) > 0 {
		// Something we may have been holding an upload for is free again. Nudge a
		// pass rather than leaving it until the hourly full walk.
		e.TriggerSync()
	}
	for _, f := range fresh {
		// The activation args land in dispatchToastActivation: a click opens
		// the sync status window on its "In use" tab.
		e.toast("File in use", f.Summary(), "action=inuse")
	}
}

// SubscribeBlocked returns a channel signalled when the blocked set changes.
func (e *Engine) SubscribeBlocked() <-chan struct{} {
	ch := make(chan struct{}, 1)
	e.blockedMu.Lock()
	e.blockedSubs = append(e.blockedSubs, ch)
	e.blockedMu.Unlock()
	return ch
}

// BlacklistPath records a file as never-sync and removes it from the blocked set.
func (e *Engine) BlacklistPath(abs string) error {
	if err := e.dirs.AddBlacklist(abs); err != nil {
		return err
	}
	e.removeBlocked(abs)
	return nil
}

// RenameBlocked renames a blocked local file to newName (in the same directory),
// drops it from the blocked set, and triggers a sync so the renamed file uploads.
func (e *Engine) RenameBlocked(abs, newName string) error {
	dst := filepath.Join(filepath.Dir(abs), newName)
	if err := os.Rename(abs, dst); err != nil {
		return err
	}
	e.removeBlocked(abs)
	e.TriggerSync()
	return nil
}

// DeleteBlocked deletes a blocked file (or folder) from disk. A blocked file can't
// sync because the server rejects its name; deleting it is the alternative to
// renaming or blacklisting. It has no baseline (never synced), so removing it
// locally is the whole operation — nothing propagates to the server.
func (e *Engine) DeleteBlocked(abs string) error {
	if err := os.RemoveAll(abs); err != nil && !os.IsNotExist(err) {
		return err
	}
	e.removeBlocked(abs)
	e.TriggerSync()
	return nil
}

// DeleteAllBlocked deletes every currently-blocked file/folder from disk in one go
// and returns how many were removed. Any that fail to delete stay in the list.
func (e *Engine) DeleteAllBlocked() (int, error) {
	e.blockedMu.Lock()
	type item struct{ dir, rel string }
	var items []item
	for dir, list := range e.blocked {
		for _, b := range list {
			items = append(items, item{dir, b.Path})
		}
	}
	e.blockedMu.Unlock()

	deleted := make(map[string]bool, len(items))
	n := 0
	var firstErr error
	for _, it := range items {
		abs := filepath.Join(it.dir, filepath.FromSlash(it.rel))
		if err := os.RemoveAll(abs); err != nil && !os.IsNotExist(err) {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		deleted[config.PathKey(abs)] = true
		n++
	}

	// Drop the deleted entries in one pass, then notify once.
	e.blockedMu.Lock()
	for dir, list := range e.blocked {
		kept := list[:0]
		for _, b := range list {
			if !deleted[config.PathKey(filepath.Join(dir, filepath.FromSlash(b.Path)))] {
				kept = append(kept, b)
			}
		}
		if len(kept) == 0 {
			delete(e.blocked, dir)
		} else {
			e.blocked[dir] = kept
		}
	}
	subs := append([]chan struct{}(nil), e.blockedSubs...)
	e.blockedMu.Unlock()
	notifyAll(subs)

	if n > 0 {
		e.TriggerSync()
	}
	return n, firstErr
}

// setBlocked replaces a pair's blocked list — used by a FULL reconcile, which
// examines the whole tree and is therefore authoritative.
func (e *Engine) setBlocked(localDir string, b []engine.Blocked) {
	e.updateBlocked(localDir, b, true)
}

// addBlocked merges newly-found blocked files into a pair's list WITHOUT clearing
// the rest — used by a scoped/delta sync, which only looked at a few paths and so
// must not wipe blocks it never re-examined (that emptied the "Can't sync" menu).
func (e *Engine) addBlocked(localDir string, b []engine.Blocked) {
	if len(b) == 0 {
		return
	}
	e.updateBlocked(localDir, b, false)
}

func (e *Engine) updateBlocked(localDir string, b []engine.Blocked, replace bool) {
	e.blockedMu.Lock()
	old := e.blocked[localDir]
	next := b
	if !replace {
		seen := make(map[string]bool, len(old))
		next = append([]engine.Blocked(nil), old...)
		for _, x := range old {
			seen[x.Path] = true
		}
		for _, nb := range b {
			if !seen[nb.Path] {
				next = append(next, nb)
			}
		}
	}
	if len(next) == 0 {
		delete(e.blocked, localDir)
	} else {
		e.blocked[localDir] = next
	}
	subs := append([]chan struct{}(nil), e.blockedSubs...)
	e.blockedMu.Unlock()
	notifyAll(subs)

	// Toast files that newly became un-syncable.
	if e.onToast != nil {
		seen := make(map[string]bool, len(old))
		for _, x := range old {
			seen[x.Path] = true
		}
		for _, nb := range b {
			if !seen[nb.Path] {
				e.toast("Can't sync "+filepath.Base(nb.Path), nb.Reason, "")
			}
		}
	}
}

// removeBlocked drops any blocked entry whose absolute path matches abs.
func (e *Engine) removeBlocked(abs string) {
	key := config.PathKey(abs)
	e.blockedMu.Lock()
	for dir, list := range e.blocked {
		kept := list[:0]
		for _, b := range list {
			if config.PathKey(filepath.Join(dir, filepath.FromSlash(b.Path))) != key {
				kept = append(kept, b)
			}
		}
		if len(kept) == 0 {
			delete(e.blocked, dir)
		} else {
			e.blocked[dir] = kept
		}
	}
	subs := append([]chan struct{}(nil), e.blockedSubs...)
	e.blockedMu.Unlock()
	notifyAll(subs)
}

// notifyAll signals each channel without blocking.
func notifyAll(subs []chan struct{}) {
	for _, ch := range subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// --- Deferred conflicts (PolicyAsk) ---

// PendingConflicts returns all conflicts awaiting a user decision.
func (e *Engine) PendingConflicts() []ConflictItem {
	e.conflictMu.Lock()
	defer e.conflictMu.Unlock()
	var out []ConflictItem
	for _, list := range e.conflicts {
		out = append(out, list...)
	}
	return out
}

// SubscribeConflicts returns a channel signalled when pending conflicts change.
func (e *Engine) SubscribeConflicts() <-chan struct{} {
	ch := make(chan struct{}, 1)
	e.conflictMu.Lock()
	e.conflictSubs = append(e.conflictSubs, ch)
	e.conflictMu.Unlock()
	return ch
}

// ResolveConflict applies a user's choice to a deferred conflict.
func (e *Engine) ResolveConflict(ctx context.Context, item ConflictItem, choice transfer.Choice) error {
	st, err := e.getStore()
	if err != nil {
		return err
	}

	ex := &transfer.Executor{
		Client:     e.client,
		State:      st,
		PairKey:    PairKey(item.LocalDir, item.RemoteRoot),
		LocalRoot:  item.LocalDir,
		RemoteRoot: item.RemoteRoot,
		Escaper:    e.escaper.Load(),
	}
	if err := ex.ApplyChoice(ctx, item.Path, choice); err != nil {
		return err
	}
	e.removeConflict(item.LocalDir, item.Path)
	return nil
}

func (e *Engine) setConflicts(p Pair, infos []transfer.ConflictInfo) {
	items := make([]ConflictItem, 0, len(infos))
	for _, in := range infos {
		items = append(items, ConflictItem{
			LocalDir: p.LocalDir, RemoteRoot: p.RemoteRoot, Path: in.Path,
			Kind: in.Kind, LocalExists: in.LocalExists, RemoteExists: in.RemoteExists,
			LocalSize: in.LocalSize, LocalMTime: in.LocalMTime,
			RemoteSize: in.RemoteSize, RemoteMTime: in.RemoteMTime,
		})
	}
	e.conflictMu.Lock()
	old := e.conflicts[p.LocalDir]
	if len(items) == 0 {
		delete(e.conflicts, p.LocalDir)
	} else {
		e.conflicts[p.LocalDir] = items
	}
	subs := append([]chan struct{}(nil), e.conflictSubs...)
	e.conflictMu.Unlock()
	notifyAll(subs)

	// Toast conflicts that weren't already pending for this pair.
	if e.onToast != nil {
		seen := make(map[string]bool, len(old))
		for _, c := range old {
			seen[c.Path] = true
		}
		for _, c := range items {
			if !seen[c.Path] {
				e.toast("Sync conflict", filepath.Base(c.Path)+" needs your decision", "")
			}
		}
	}
}

func (e *Engine) removeConflict(localDir, rel string) {
	e.conflictMu.Lock()
	list := e.conflicts[localDir]
	kept := list[:0]
	for _, c := range list {
		if c.Path != rel {
			kept = append(kept, c)
		}
	}
	if len(kept) == 0 {
		delete(e.conflicts, localDir)
	} else {
		e.conflicts[localDir] = kept
	}
	subs := append([]chan struct{}(nil), e.conflictSubs...)
	e.conflictMu.Unlock()
	notifyAll(subs)
}

// --- Browser, sharing, settings, ignore (GUI pass-throughs) ---

// Browse lists a remote directory (one level) for the file browser.
func (e *Engine) Browse(ctx context.Context, remotePath string) ([]transport.Entry, error) {
	return e.client.PropFind(ctx, remotePath, 1)
}

// Escaper returns the current name-escaping translator (nil-safe to use: a
// nil or inactive escaper encodes/decodes to identity). The adopt scan needs
// it to classify local names against their escaped server counterparts.
func (e *Engine) Escaper() *engine.Escaper { return e.escaper.Load() }

// GlobalIgnoreMatcher returns the account's global ignore predicate — the same
// rules live-mode pairs apply, minus per-pair excludes (on-demand mode has no
// pairs). The adopt scan uses it so ignored trees (node_modules, .git, …) never
// enter the plan: without it they classify as "upload" and the confirm dialog
// offers to push gigabytes of dev-tree junk to the server.
func (e *Engine) GlobalIgnoreMatcher() func(rel string) bool {
	gi, _ := e.dirs.LoadIgnore()
	return engine.NewIgnore(gi).Match
}

// RemoteTree walks the whole remote subtree under root and returns its current
// state keyed by root-relative path. Used by the on-demand adopt scan, which
// must classify every pre-existing local file against the server before it can
// show the user a summary.
//
// Names are reported RAW — no escaper decoding — because the on-demand world
// (placeholder identities, reconcile, write-back) works in raw server names,
// and adopt must classify in that same namespace: decoding here would mark a
// local file in-sync under an identity reconcile can never find, which reads
// as a server-side delete. An escaped-name file simply fails its upload (the
// server forbids the raw name) and stays a plain local file — safe.
//
// The crawl is checkpoint-backed: each directory listing is cached in the
// state DB as it is fetched, so a cancelled or failed scan retried later only
// re-fetches what changed — on a big account that turns a minutes-long recrawl
// into seconds. Rows are keyed apart from the sync pairs' checkpoints and are
// deliberately NOT cleared on success: a crash mid-adopt is recovered by
// re-scanning, which should stay warm. The 14-day age-out is the backstop.
//
// skip (optional) is an ignore predicate (root-relative paths): matches are
// omitted from the result AND not descended into server-side, so ignored trees
// cost no PROPFINDs. progress (optional) receives the running count of
// directories listed — the heartbeat a UI needs to distinguish a long crawl
// from a hang.
func (e *Engine) RemoteTree(ctx context.Context, root string, skip func(string) bool, progress func(int)) (map[string]engine.RemoteState, error) {
	opts := engine.ScanOpts{Skip: skip, Progress: progress}
	var cp *scanCheckpoint
	if st, err := e.getStore(); err == nil {
		cp = newScanCheckpoint(st, "vfs-adopt:"+strings.Trim(root, "/"))
		opts.Checkpoint = cp
	}
	remote, err := engine.RemoteScan(ctx, e.client, root, opts)
	if cp != nil {
		cp.logSummary()
	}
	return remote, err
}

// ListShares / CreatePublicLink / CreateUserShare / DeleteShare proxy the client.
func (e *Engine) ListShares(ctx context.Context, path string) ([]transport.Share, error) {
	return e.client.ListShares(ctx, path)
}
func (e *Engine) CreatePublicLink(ctx context.Context, path string, opt transport.PublicLinkOptions) (transport.Share, error) {
	return e.client.CreatePublicLink(ctx, path, opt)
}
func (e *Engine) CreateUserShare(ctx context.Context, path, user string, perms int) (transport.Share, error) {
	return e.client.CreateUserShare(ctx, path, user, perms)
}
func (e *Engine) DeleteShare(ctx context.Context, id string) error {
	return e.client.DeleteShare(ctx, id)
}

// Limits returns the current bandwidth limits (KiB/s).
func (e *Engine) Limits() (up, down int) {
	s, _ := e.dirs.LoadSettings()
	return s.UploadKBps, s.DownloadKBps
}

// SetLimits persists and applies bandwidth limits (KiB/s; 0 = unlimited).
func (e *Engine) SetLimits(up, down int) error {
	if err := e.dirs.UpdateSettings(func(s *config.Settings) {
		s.UploadKBps, s.DownloadKBps = up, down
	}); err != nil {
		return err
	}
	e.client.SetLimits(up, down)
	return nil
}

// BaseDir returns the local root for newly-synced account folders, defaulting to
// ~/Nextcloud if unset.
func (e *Engine) BaseDir() string {
	s, _ := e.dirs.LoadSettings()
	if s.BaseDir != "" {
		return s.BaseDir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Nextcloud")
}

// SetBaseDir persists the local base directory.
func (e *Engine) SetBaseDir(dir string) error {
	return e.dirs.UpdateSettings(func(s *config.Settings) { s.BaseDir = dir })
}

// SyncedRemotes returns the set of remote folder paths currently configured as
// sync pairs (so a UI can show which folders are synced).
func (e *Engine) SyncedRemotes() map[string]bool {
	pairs, _ := e.dirs.LoadPairs()
	out := make(map[string]bool, len(pairs))
	for _, p := range pairs {
		out[strings.Trim(p.RemoteRoot, "/")] = true
	}
	return out
}

// AddSyncFolder starts syncing a remote folder: it creates a sync pair mapping
// the remote path to <BaseDir>/<path> and begins watching it immediately.
func (e *Engine) AddSyncFolder(remoteRoot string) error {
	remoteRoot = strings.Trim(remoteRoot, "/")
	local := filepath.Join(e.BaseDir(), filepath.FromSlash(remoteRoot))

	pairs, err := e.dirs.LoadPairs()
	if err != nil {
		return err
	}
	for _, p := range pairs {
		if strings.Trim(p.RemoteRoot, "/") == remoteRoot {
			return nil // already synced
		}
	}
	if err := e.forgetCloneStatus(local, remoteRoot); err != nil {
		return err
	}
	pairs = append(pairs, config.SyncPair{LocalDir: local, RemoteRoot: remoteRoot})
	if err := e.dirs.SavePairs(pairs); err != nil {
		return err
	}
	return e.ReloadPairs()
}

// AddSyncPair starts syncing a remote folder to an explicit local directory
// (rather than the default <BaseDir>/<path>), enabling multiple sync
// connections that target different locations. The local directory is created
// if it doesn't exist.
func (e *Engine) AddSyncPair(localDir, remoteRoot string) error {
	remoteRoot = strings.Trim(remoteRoot, "/")
	localDir = filepath.Clean(strings.TrimSpace(localDir))
	if localDir == "" || localDir == "." || !filepath.IsAbs(localDir) {
		return fmt.Errorf("local folder must be a full path")
	}
	pairs, err := e.dirs.LoadPairs()
	if err != nil {
		return err
	}
	for _, p := range pairs {
		// The EXACT pair already existing is success, not an error: signing
		// back into an account whose config survived (only the keychain secret
		// was lost) replays the setup flow, and "that remote folder is already
		// synced" dead-ended it (2026-08-21).
		if strings.Trim(p.RemoteRoot, "/") == remoteRoot && filepath.Clean(p.LocalDir) == localDir {
			return nil
		}
		if strings.Trim(p.RemoteRoot, "/") == remoteRoot {
			return fmt.Errorf("that remote folder is already synced")
		}
		if filepath.Clean(p.LocalDir) == localDir {
			return fmt.Errorf("that local folder is already used by another sync")
		}
	}
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		return fmt.Errorf("create local folder: %w", err)
	}
	if err := e.forgetCloneStatus(localDir, remoteRoot); err != nil {
		return err
	}
	pairs = append(pairs, config.SyncPair{LocalDir: localDir, RemoteRoot: remoteRoot})
	if err := e.dirs.SavePairs(pairs); err != nil {
		return err
	}
	return e.ReloadPairs()
}

// forgetCloneStatus drops any clone-state row already sitting under a pair
// that is being ADDED. Rows written before removal/reset started clearing
// them outlive their pair, and a leftover "started" would make the new pair's
// first sync a clone RESUME — refetching (overwriting) any local file whose
// size differs from the server — where a fresh pair takes over and never
// overwrites. Resume is only right for an interrupted clone of a pair that is
// still configured; a folder the user adds is new from their point of view
// (GitHub #4 left such a row behind for a populated folder). Runs BEFORE the
// pair is saved, so a failure adds nothing rather than adding it unprotected.
func (e *Engine) forgetCloneStatus(localDir, remoteRoot string) error {
	st, err := e.getStore()
	if err != nil {
		return fmt.Errorf("prepare folder state: %w", err)
	}
	if err := st.ClearCloneStatus(PairKey(localDir, remoteRoot)); err != nil {
		return fmt.Errorf("prepare folder state: %w", err)
	}
	return nil
}

// Move/sync mutual exclusion (see the moveExcl field). A sync pass brackets its
// run with beginSyncPass/endSyncPass; a folder move brackets its run with
// beginMove/endMove. Both acquisitions are non-blocking: beginSyncPass returns
// false when a move is in progress (the pass must skip and NOT call endSyncPass),
// and beginMove returns false when any sync pass is running (the move must be
// refused). They can therefore never overlap.
func (e *Engine) beginSyncPass() bool { return e.moveExcl.TryRLock() }
func (e *Engine) endSyncPass()        { e.moveExcl.RUnlock() }
func (e *Engine) beginMove() bool     { return e.moveExcl.TryLock() }
func (e *Engine) endMove()            { e.moveExcl.Unlock() }

// MoveSyncPair re-points an existing pair from oldLocal to newLocal WITHOUT
// re-transferring, running relocate() to move the files on disk at the one safe
// moment. The baseline pair_key is derived from the local path (see PairKey), so
// a move would otherwise orphan the baseline and trigger a full re-clone; this
// re-keys it instead.
//
// ORDER IS SAFETY-CRITICAL. The original version moved the files while the old
// folder was still being watched, so the watcher saw every file vanish and
// propagated mass DELETIONS to the server. The protection is now structural: a
// move and a sync are mutually exclusive (see moveExcl). We take the move lock
// FIRST — and if a sync pass is running we refuse outright rather than race it —
// so by the time we stop the watcher and relocate, no sync can be looking at the
// folder. Stopping the watcher also waits for its goroutine to exit, releasing
// the folder handle so the rename can't hit a sharing violation. Config flips to
// the new path only AFTER the relocate succeeds, so a crash mid-move leaves the
// original intact.
func (e *Engine) MoveSyncPair(oldLocal, newLocal string, relocate func() error) error {
	oldLocal = filepath.Clean(strings.TrimSpace(oldLocal))
	newLocal = filepath.Clean(strings.TrimSpace(newLocal))
	if newLocal == "" || newLocal == "." || !filepath.IsAbs(newLocal) {
		return fmt.Errorf("new local folder must be a full path")
	}

	// Take exclusive control up front. If a sync pass is running, beginMove fails
	// and we refuse — nothing has changed on disk, so the user just retries once
	// the sync finishes. This is the guarantee that a move never overlaps a sync.
	if !e.beginMove() {
		return fmt.Errorf("a sync is in progress — try the move again when it finishes")
	}
	released := false
	release := func() {
		if !released {
			released = true
			e.endMove()
		}
	}
	defer release() // safety net; the paths below release explicitly before reloading watchers

	pairs, err := e.dirs.LoadPairs()
	if err != nil {
		return err
	}
	idx := -1
	for i, p := range pairs {
		if filepath.Clean(p.LocalDir) == oldLocal {
			idx = i
		}
	}
	if idx < 0 {
		return fmt.Errorf("no sync folder at %s", oldLocal)
	}
	for i, p := range pairs {
		if i != idx && filepath.Clean(p.LocalDir) == newLocal {
			return fmt.Errorf("that local folder is already used by another sync")
		}
	}
	remoteRoot := pairs[idx].RemoteRoot
	oldKey := PairKey(oldLocal, remoteRoot)
	newKey := PairKey(newLocal, remoteRoot)

	// Surface the move in the flyout. No sync is running (we hold the move lock),
	// so the status line is free to show it; the confirming sync after the move
	// resets it to the normal "Up to date".
	e.status("Moving your folder…")

	// Stop the pair's watcher and wait for its goroutine to exit (releasing the
	// folder handle). Nothing is mid-sync — beginMove only succeeded because no
	// sync was running, and new syncs skip while we hold the move lock.
	e.stopWatcherSync(oldKey)

	// On any failure from here on, release the lock and restart the watcher for
	// whatever config still says — the original folder, since SavePairs runs only
	// on the success path — then return the error.
	fail := func(err error) error {
		release()
		_ = e.ReloadPairs()
		return err
	}

	// Move the files on disk. Nothing watches the old folder now.
	if relocate != nil {
		if err := relocate(); err != nil {
			return fail(err)
		}
	}

	// Re-key the baseline to the new path so the moved folder keeps its synced
	// state instead of re-cloning.
	if oldKey != newKey {
		st, err := e.getStore()
		if err != nil {
			return fail(err)
		}
		if err := st.RekeyPair(oldKey, newKey); err != nil {
			return fail(fmt.Errorf("re-key baseline: %w", err))
		}
	}

	// Only now flip config to the new folder (a crash before here ⇒ original intact).
	pairs[idx].LocalDir = newLocal
	if err := e.dirs.SavePairs(pairs); err != nil {
		return fail(err)
	}

	// Release the exclusion BEFORE restarting the watcher, so the new pair's
	// initial/confirming sync isn't skipped by its own move guard.
	release()
	return e.ReloadPairs()
}

// RemoveSyncFolder stops syncing a remote folder (removes its pair and watcher).
// When deleteLocal is true the local copy is removed too; otherwise it's left in
// place.
func (e *Engine) RemoveSyncFolder(remoteRoot string, deleteLocal bool) error {
	remoteRoot = strings.Trim(remoteRoot, "/")
	pairs, err := e.dirs.LoadPairs()
	if err != nil {
		return err
	}
	var removed *config.SyncPair
	out := pairs[:0]
	for _, p := range pairs {
		if strings.Trim(p.RemoteRoot, "/") == remoteRoot {
			p := p
			removed = &p
			continue
		}
		out = append(out, p)
	}
	if err := e.dirs.SavePairs(out); err != nil {
		return err
	}
	if err := e.ReloadPairs(); err != nil { // stops the watcher first
		return err
	}
	if removed != nil {
		if st, serr := e.getStore(); serr == nil {
			pk := PairKey(removed.LocalDir, removed.RemoteRoot)
			// Drop the pair's checkpoint rows — cached dir listings are the big
			// blobs. Best-effort; the 14-day age-out is the backstop.
			if cerr := st.ClearScanCheckpoint(pk); cerr != nil {
				slog.Warn("scan checkpoint clear on remove failed", "err", cerr)
			}
			// And its clone state. The key is (local dir, remote root), so
			// re-adding the same folder later finds this row again — and a
			// leftover "started" turns that re-add into a clone RESUME, which
			// overwrites any local file whose size differs from the server,
			// where a fresh pair takes over and never overwrites (GitHub #4).
			if cerr := st.ClearCloneStatus(pk); cerr != nil {
				slog.Warn("clone status clear on remove failed — re-adding this folder would resume, not take over", "err", cerr)
			}
		}
	}
	if deleteLocal && removed != nil {
		return os.RemoveAll(removed.LocalDir)
	}
	return nil
}

// ResetPairState wipes a pair's baseline and scan-checkpoint rows so its next
// sync treats the pair as brand new: server files download, nothing is inferred
// deleted. This is the "start from scratch" safety step and MUST run BEFORE the
// pair's local files are deleted — an empty folder against a surviving baseline
// reads as the user having deleted everything, and those deletes would
// propagate to the server.
func (e *Engine) ResetPairState(localDir, remoteRoot string) error {
	st, err := e.getStore()
	if err != nil {
		return err
	}
	key := PairKey(localDir, remoteRoot)
	if err := st.DeleteBaselineAll(key); err != nil {
		return err
	}
	// "Brand new" must include the clone state: a leftover "started" would
	// make the next sync a clone RESUME (differing local files refetched, i.e.
	// overwritten) rather than a takeover (never overwrites).
	if err := st.ClearCloneStatus(key); err != nil {
		return err
	}
	if err := st.ClearScanCheckpoint(key); err != nil {
		slog.Warn("scan checkpoint clear on reset failed", "err", err)
	}
	return nil
}

// RemotePathFor maps an absolute local path to its files-root-relative remote
// path by finding the sync pair that contains it. Returns false if the path
// isn't inside any synced folder.
func (e *Engine) RemotePathFor(localAbs string) (string, bool) {
	localAbs = filepath.Clean(localAbs)
	pairs, err := e.dirs.LoadPairs()
	if err != nil {
		return "", false
	}
	for _, p := range pairs {
		rel, err := filepath.Rel(filepath.Clean(p.LocalDir), localAbs)
		if err != nil {
			continue
		}
		rel = filepath.ToSlash(rel)
		if rel == ".." || strings.HasPrefix(rel, "../") {
			continue // outside this pair
		}
		root := strings.Trim(p.RemoteRoot, "/")
		if rel == "." {
			return root, true
		}
		if root == "" {
			return rel, true
		}
		return root + "/" + rel, true
	}
	return "", false
}

// SetOverlayRefresh registers a callback invoked with an absolute path whenever
// that path's sync state changes, so the shell can refresh its overlay icon.
func (e *Engine) SetOverlayRefresh(fn func(string)) { e.onOverlayRefresh = fn }

func (e *Engine) markInflight(abs string, on bool) {
	e.inflightMu.Lock()
	if e.inflight == nil {
		e.inflight = map[string]bool{}
	}
	if on {
		e.inflight[abs] = true
	} else {
		delete(e.inflight, abs)
	}
	e.inflightMu.Unlock()
	if e.onOverlayRefresh != nil {
		e.onOverlayRefresh(abs)
	}
}

// FileStatus reports the sync state of an absolute local path for shell overlay
// icons: "ok" (synced), "sync" (transferring), "warn" (can't sync / conflict),
// or "none" (outside any synced folder).
func (e *Engine) FileStatus(abs string) string {
	abs = filepath.Clean(abs)
	under := func(dir string) bool {
		rel, err := filepath.Rel(filepath.Clean(dir), abs)
		if err != nil {
			return false
		}
		rel = filepath.ToSlash(rel)
		return rel != ".." && !strings.HasPrefix(rel, "../")
	}

	inPair := false
	pairDir := ""
	pairRemote := ""
	pairs, _ := e.dirs.LoadPairs()
	for _, p := range pairs {
		if under(p.LocalDir) {
			inPair = true
			pairDir = p.LocalDir
			pairRemote = strings.Trim(p.RemoteRoot, "/")
			break
		}
	}
	// Engine-ignored files never sync, so no badge may claim otherwise (#575):
	// a tick on the official client's journals would be a lie, and "sync" or
	// "warn" can never truthfully apply either.
	if inPair {
		if rel, err := filepath.Rel(filepath.Clean(pairDir), abs); err == nil {
			if e.GlobalIgnoreMatcher()(filepath.ToSlash(rel)) {
				return "none"
			}
		}
	}
	// On-demand mounts are ours too (they have no pairs — entering the mode
	// clears them all), but ONLY for states the Cloud Files platform cannot
	// know: transfers and errors. Per-item AVAILABILITY there — online-only
	// cloud, downloaded tick, pinned solid — is the platform's own glyph set,
	// and a blanket "ok" badge would stamp a synced tick over online-only
	// files, erasing exactly the distinction virtual files exist to show.
	inMount := false
	mountDir := ""
	if !inPair {
		e.overlayRootsMu.RLock()
		for _, dir := range e.overlayRoots {
			if under(dir) {
				inMount = true
				mountDir = dir
				break
			}
		}
		e.overlayRootsMu.RUnlock()
	}
	if !inPair && !inMount {
		return "none"
	}
	e.inflightMu.Lock()
	syncing := e.inflight[abs]
	e.inflightMu.Unlock()
	if syncing {
		return "sync"
	}
	for _, b := range e.BlockedFiles() {
		if filepath.Clean(b.Abs) == abs {
			return "warn"
		}
	}
	for _, c := range e.PendingConflicts() {
		if filepath.Clean(filepath.Join(c.LocalDir, filepath.FromSlash(c.Path))) == abs {
			return "warn"
		}
	}
	// A share on this exact remote path marks the item as shared — the share
	// ROOT only, not everything inside it (OneDrive's model). It outranks the
	// idle answers ("ok"/mount "none") but never a transfer or a problem, and
	// it applies inside mounts too: availability glyphs are native there,
	// sharing is not.
	root, base := pairDir, pairRemote
	if inMount {
		root, base = mountDir, "" // account mounts sit at the files root
	}
	if rel, err := filepath.Rel(filepath.Clean(root), abs); err == nil {
		remote := filepath.ToSlash(rel)
		if remote == "." {
			remote = ""
		}
		if base != "" {
			remote = strings.Trim(base+"/"+remote, "/")
		}
		if e.isSharedRemote(remote) {
			return "shared"
		}
	}
	if inMount {
		return "none" // idle in a mount: the native cloud glyph owns the row
	}
	return "ok"
}

// SetSharedRemotePaths replaces the set of files-root-relative paths that
// carry a share (created by this user or received from another). FileStatus
// answers "shared" for exactly these nodes.
func (e *Engine) SetSharedRemotePaths(paths []string) {
	m := make(map[string]bool, len(paths))
	for _, p := range paths {
		p = strings.Trim(strings.ReplaceAll(p, "\\", "/"), "/")
		if p != "" {
			m[p] = true
		}
	}
	e.sharedMu.Lock()
	e.sharedRemote = m
	e.sharedMu.Unlock()
}

func (e *Engine) isSharedRemote(remote string) bool {
	if remote == "" {
		return false
	}
	e.sharedMu.RLock()
	defer e.sharedMu.RUnlock()
	return e.sharedRemote[remote]
}

// RefreshShares fetches the account's share list — both created and received —
// and updates the set FileStatus answers "shared" from. Nodes whose shared
// state flipped get an overlay refresh so Explorer repaints them, and a share
// that ARRIVES from another user (after the first refresh has primed the set)
// raises a toast so the recipient hears about it.
func (e *Engine) RefreshShares(ctx context.Context) error {
	if e.client == nil {
		return nil
	}
	own, received, err := e.client.ListAllShares(ctx)
	if err != nil {
		return err
	}
	norm := func(p string) string { return strings.Trim(strings.ReplaceAll(p, "\\", "/"), "/") }
	set := make(map[string]bool, len(own)+len(received))
	recv := make(map[string]bool, len(received))
	for _, s := range own {
		if p := norm(s.Path); p != "" {
			set[p] = true
		}
	}
	type arrival struct{ path, by string }
	var arrived []arrival
	e.sharedMu.Lock()
	old := e.sharedRemote
	oldRecv := e.receivedShares
	primed := e.sharesPrimed
	for _, s := range received {
		p := norm(s.Path)
		if p == "" {
			continue
		}
		set[p] = true
		recv[p] = true
		if primed && !oldRecv[p] {
			arrived = append(arrived, arrival{path: p, by: s.SharedBy()})
		}
	}
	e.sharedRemote = set
	e.receivedShares = recv
	e.sharesPrimed = true
	e.sharedMu.Unlock()

	for _, a := range arrived {
		name := a.path
		if i := strings.LastIndexByte(name, '/'); i >= 0 {
			name = name[i+1:]
		}
		e.toast("Folder shared with you", a.by+" shared \""+name+"\" with you — it appears in your sync folder", "")
	}
	if e.onOverlayRefresh == nil {
		return nil
	}
	poke := func(remote string) {
		for _, abs := range e.localPathsForRemote(remote) {
			e.onOverlayRefresh(abs)
		}
	}
	for p := range set {
		if !old[p] {
			poke(p)
		}
	}
	for p := range old {
		if !set[p] {
			poke(p)
		}
	}
	return nil
}

// localPathsForRemote maps a files-root-relative path to every local absolute
// path that shows it, through the live pairs and the on-demand mounts.
func (e *Engine) localPathsForRemote(remote string) []string {
	var out []string
	remote = strings.Trim(remote, "/")
	if pairs, err := e.dirs.LoadPairs(); err == nil {
		for _, p := range pairs {
			base := strings.Trim(p.RemoteRoot, "/")
			rest := ""
			switch {
			case base == "":
				rest = remote
			case remote == base:
				rest = ""
			case strings.HasPrefix(remote, base+"/"):
				rest = remote[len(base)+1:]
			default:
				continue
			}
			out = append(out, filepath.Join(p.LocalDir, filepath.FromSlash(rest)))
		}
	}
	e.overlayRootsMu.RLock()
	for _, dir := range e.overlayRoots {
		out = append(out, filepath.Join(dir, filepath.FromSlash(remote)))
	}
	e.overlayRootsMu.RUnlock()
	return out
}

// presenceLoop keeps the user's Nextcloud presence dot "online" while the
// engine runs. Without heartbeats the server decays a user to offline within
// minutes of their last web/app activity — which made accounts that only run
// Nimbo look offline to everyone despite syncing happily. The official client
// heartbeats the same way. Best-effort: servers without the user_status app
// just error quietly.
func (e *Engine) presenceLoop(ctx context.Context) {
	beat := func() {
		bctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		if err := e.client.UserStatusHeartbeat(bctx); err != nil {
			slog.Debug("user-status heartbeat failed", "err", err)
		}
	}
	beat()
	t := time.NewTicker(4 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			beat()
		}
	}
}

// sharesRefreshLoop refreshes the shared-paths set once at engine start and
// then periodically — shares change server-side with no local activity to
// piggyback on, so a poll is the only honest source.
func (e *Engine) sharesRefreshLoop(ctx context.Context) {
	refresh := func() {
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := e.RefreshShares(rctx); err != nil {
			slog.Debug("share list refresh failed", "err", err)
		}
	}
	refresh()
	// 30 minutes, not 5: this poll was 43% of an idle client's server traffic
	// (#599). Freshness for the common case — a share arriving — comes from
	// refreshSharesSoon riding the server's own notify_notification push; this
	// ticker only covers servers without push and shares that raise no
	// notification (e.g. created by yourself elsewhere).
	t := time.NewTicker(30 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			refresh()
		}
	}
}

// sharesEventThrottle coalesces refreshSharesSoon calls: notification events
// arrive in bursts, and one refresh per window is plenty for share markers.
const sharesEventThrottle = time.Minute

// refreshSharesSoon refreshes the shared-paths set unless it already ran
// within sharesEventThrottle. Fired off notify_notification pushes — a new
// incoming share raises a notification, so this keeps share markers and
// arrival toasts prompt while the periodic poll idles at 30 minutes.
func (e *Engine) refreshSharesSoon(ctx context.Context) {
	e.sharedMu.Lock()
	if time.Since(e.sharesRefreshedAt) < sharesEventThrottle {
		e.sharedMu.Unlock()
		return
	}
	e.sharesRefreshedAt = time.Now() // claim before the fetch so a burst coalesces
	e.sharedMu.Unlock()
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := e.RefreshShares(rctx); err != nil {
		slog.Debug("event-driven share refresh failed", "err", err)
	}
}

// SetOverlayRoots records folders that FileStatus should treat as synced
// territory even though no live sync pair covers them.
//
// This exists for on-demand (virtual files) mode, which deliberately has no
// pairs at all: switching into it clears them so the watcher cannot fight the
// Cloud Files provider. FileStatus keys off pairs, so without this the Explorer
// shell extension is told "none" for every file in the mount and draws no
// status icon anywhere.
func (e *Engine) SetOverlayRoots(dirs []string) {
	if e == nil {
		return
	}
	cp := append([]string(nil), dirs...)
	e.overlayRootsMu.Lock()
	e.overlayRoots = cp
	e.overlayRootsMu.Unlock()
}

// SetProgressFunc registers a callback notified (throttled) with live sync
// progress. Used by the GUI to drive a progress display.
func (e *Engine) SetProgressFunc(f func(SyncProgress)) { e.onProgress = f }

// SetToastFunc registers a callback for desktop notifications (sync conflicts,
// can't-sync files, sync errors). Only callers that want toasts (the GUI) set
// it; the CLI leaves it nil.
func (e *Engine) SetToastFunc(f func(title, message, link string)) { e.onToast = f }

// noteEncrypted surfaces a skipped end-to-end encrypted folder, once per path
// per engine lifetime: E2EE contents are opaque without the client keys, so
// Nimbo leaves those folders alone — but silently not syncing needs explaining.
func (e *Engine) noteEncrypted(rel string) {
	e.encMu.Lock()
	if e.encSeen == nil {
		e.encSeen = map[string]bool{}
	}
	seen := e.encSeen[rel]
	e.encSeen[rel] = true
	e.encMu.Unlock()
	if seen {
		return
	}
	slog.Info("skipping end-to-end encrypted folder (not supported)", "path", rel)
	e.toast("Encrypted folder skipped",
		"\""+rel+"\" is end-to-end encrypted — Nimbo can't sync it and is leaving it alone.", "")
}

func (e *Engine) toast(title, message, link string) {
	if e.onToast != nil {
		e.onToast(title, message, link)
	}
}

// SetAuthLostFunc registers a callback invoked once when the server rejects the
// stored credentials (so the GUI can prompt re-authentication).
func (e *Engine) SetAuthLostFunc(f func()) { e.onAuthLost = f }

// SetFilesChangedFunc registers a callback invoked when notify_push reports a
// server-side file change. On-demand mode uses it to reconcile placeholders
// immediately (the engine's own pairs are inactive in that mode).
func (e *Engine) SetFilesChangedFunc(f func()) { e.onFilesChanged = f }

func (e *Engine) authLost() {
	e.toastMu.Lock()
	fired := e.authLostFired
	e.authLostFired = true
	e.toastMu.Unlock()
	if !fired && e.onAuthLost != nil {
		e.onAuthLost()
	}
}

func (e *Engine) resetAuthLost() {
	e.toastMu.Lock()
	e.authLostFired = false
	e.toastMu.Unlock()
}

// syncErrKind classifies a sync error so the UI can show a meaningful status:
// "auth" (credentials rejected), "offline" (network unreachable), or "error".
func syncErrKind(err error) string {
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "401") || strings.Contains(s, "unauthor") || strings.Contains(s, "app password"):
		return "auth"
	case strings.Contains(s, "no such host") || strings.Contains(s, "dial ") ||
		strings.Contains(s, "connection refused") || strings.Contains(s, "timeout") ||
		strings.Contains(s, "deadline exceeded") || strings.Contains(s, "network is unreachable") ||
		strings.Contains(s, "no route to host") || strings.Contains(s, "connection reset") ||
		strings.Contains(s, "request failed after") || strings.Contains(s, "i/o timeout"):
		return "offline"
	default:
		return "error"
	}
}

// Progress returns the current sync-progress snapshot.
func (e *Engine) Progress() SyncProgress {
	e.progMu.Lock()
	defer e.progMu.Unlock()
	p := e.prog
	if p.Active {
		p.DoneBytes = e.progBytes.Load()
		// Retried/resumed transfers can re-report bytes; never show >100%.
		if p.TotalBytes > 0 && p.DoneBytes > p.TotalBytes {
			p.DoneBytes = p.TotalBytes
		}
		// ETA rides on the cumulative average rate (bytes since the burst began),
		// which is far steadier than the instantaneous speed — it doesn't whipsaw
		// between a big file and a burst of tiny ones, and converges as it runs.
		if !e.progStartAt.IsZero() {
			if el := time.Since(e.progStartAt).Seconds(); el > 0 {
				p.AvgSpeed = int64(float64(p.DoneBytes) / el)
			}
		}
	}
	return p
}

func (e *Engine) emitProgress() {
	if e.onProgress != nil {
		e.onProgress(e.Progress())
	}
}

// progStart begins (or joins) a progress burst contributing total transfers and
// bytes. A clone passes 0/0 and grows the totals with progAddTotal as it
// enumerates.
func (e *Engine) progStart(total int, bytes int64) {
	e.progMu.Lock()
	first := e.progRuns == 0
	if first {
		e.prog = SyncProgress{Active: true}
		e.progBytes.Store(0)
		e.progStartAt = time.Now()
		e.progStop = make(chan struct{})
	}
	e.progRuns++
	e.prog.Total += total
	e.prog.TotalBytes += bytes
	stop := e.progStop
	e.progMu.Unlock()
	if first {
		go e.speedLoop(stop)
	}
	e.emitProgress()
}

// progAddTotal grows the planned totals mid-burst — used by the initial clone,
// which learns the file count and byte size folder-by-folder as it enumerates.
func (e *Engine) progAddTotal(files int, bytes int64) {
	e.progMu.Lock()
	if e.progRuns > 0 {
		e.prog.Total += files
		e.prog.TotalBytes += bytes
	}
	e.progMu.Unlock()
	e.emitProgress()
}

// progEnd marks one burst run done; the last one out clears progress.
func (e *Engine) progEnd() {
	e.progMu.Lock()
	if e.progRuns > 0 {
		e.progRuns--
	}
	last := e.progRuns == 0
	if last {
		if e.progStop != nil {
			close(e.progStop)
			e.progStop = nil
		}
		e.prog = SyncProgress{Active: false}
	}
	e.progMu.Unlock()
	e.emitProgress()
}

func (e *Engine) progCurrent(name string) {
	e.progMu.Lock()
	e.prog.Current = name
	e.progMu.Unlock()
}

func (e *Engine) progComplete() {
	e.progMu.Lock()
	e.prog.Done++
	e.progMu.Unlock()
}

// speedLoop samples throughput ~twice a second and emits a throttled update.
func (e *Engine) speedLoop(stop chan struct{}) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	last := e.progBytes.Load()
	var ema float64 // smoothed bytes/sec
	// alpha ~0.08 over 500ms samples ≈ a ~6s window: responsive but not jumpy.
	const alpha = 0.08
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			cur := e.progBytes.Load()
			sample := float64(cur-last) * 2 // bytes over the 500ms window → per second
			if ema == 0 {
				ema = sample
			} else {
				ema = alpha*sample + (1-alpha)*ema
			}
			e.progMu.Lock()
			e.prog.Speed = int64(ema)
			e.progMu.Unlock()
			last = cur
			e.emitProgress()
		}
	}
}

// GlobalIgnore returns the global ignore patterns.
func (e *Engine) GlobalIgnore() ([]string, error) { return e.dirs.LoadIgnore() }

// SetGlobalIgnore persists the global ignore patterns.
func (e *Engine) SetGlobalIgnore(patterns []string) error { return e.dirs.SaveIgnore(patterns) }

// AddExclude adds a selective-sync exclude pattern to the pair at localDir.
func (e *Engine) AddExclude(localDir, pattern string) error {
	return e.editPairExcludes(localDir, func(ex []string) []string {
		for _, p := range ex {
			if p == pattern {
				return ex
			}
		}
		return append(ex, pattern)
	})
}

// RemoveExclude removes a selective-sync exclude pattern from the pair at localDir.
func (e *Engine) RemoveExclude(localDir, pattern string) error {
	return e.editPairExcludes(localDir, func(ex []string) []string {
		out := ex[:0]
		for _, p := range ex {
			if p != pattern {
				out = append(out, p)
			}
		}
		return out
	})
}

func (e *Engine) editPairExcludes(localDir string, fn func([]string) []string) error {
	pairs, err := e.dirs.LoadPairs()
	if err != nil {
		return err
	}
	for i := range pairs {
		if pairs[i].LocalDir == localDir {
			pairs[i].Excludes = fn(pairs[i].Excludes)
			return e.dirs.SavePairs(pairs)
		}
	}
	return fmt.Errorf("no sync pair for %s", localDir)
}

// excludesFor returns the pair's current selective-sync excludes from disk. A
// watcher captures its Pair by value when it starts, so its in-memory Excludes go
// stale the moment the user toggles selective sync; sync entrypoints call this so
// every pass filters against the latest excludes. ok is false when the pair can't
// be read (transient error or pair removed) — callers then keep their snapshot
// rather than syncing as if nothing were excluded.
func (e *Engine) excludesFor(localDir, remoteRoot string) ([]string, bool) {
	pairs, err := e.dirs.LoadPairs()
	if err != nil {
		return nil, false
	}
	for _, p := range pairs {
		if p.LocalDir == localDir && p.RemoteRoot == remoteRoot {
			return p.Excludes, true
		}
	}
	return nil, false
}

// DeselectFolder stops syncing rel (a pair-relative folder) within the pair at
// localDir — a selective-sync exclude. The server copy is ALWAYS kept. When
// deleteLocal is true it also removes the already-downloaded local copy to reclaim
// disk space, done in a deliberately safe order:
//
//  1. Persist the exclude first. Because sync entrypoints reload excludes
//     (excludesFor), the live and every future sync now filters rel from BOTH
//     sides, so the local removal can never be read as a deletion to propagate.
//  2. Prune the baseline under rel, so a later re-select sees those paths as
//     absent-from-baseline and re-downloads them (rather than local-absent +
//     base-present → a server delete).
//  3. Only then remove the local subtree. Any sync still in flight planned its
//     actions while the files were present, so it cannot have queued a delete.
//
// Re-selecting is just RemoveExclude (the next sync re-downloads from the server).
func (e *Engine) DeselectFolder(localDir, rel string, deleteLocal bool) error {
	rel = strings.Trim(strings.ReplaceAll(rel, "\\", "/"), "/")
	if rel == "" {
		return fmt.Errorf("empty folder")
	}
	if err := e.AddExclude(localDir, rel); err != nil {
		return err
	}
	if !deleteLocal {
		e.TriggerSync()
		return nil
	}

	pairs, err := e.dirs.LoadPairs()
	if err != nil {
		return err
	}
	remoteRoot, found := "", false
	for _, p := range pairs {
		if p.LocalDir == localDir {
			remoteRoot, found = p.RemoteRoot, true
			break
		}
	}
	if !found {
		return fmt.Errorf("no sync pair for %s", localDir)
	}

	if st, err := e.getStore(); err == nil {
		if derr := st.DeleteBaselineUnder(PairKey(localDir, remoteRoot), rel); derr != nil {
			return derr // bail before touching local files if the baseline prune failed
		}
	}
	if err := os.RemoveAll(filepath.Join(localDir, filepath.FromSlash(rel))); err != nil {
		return err
	}
	e.TriggerSync()
	return nil
}

// SetStatusFunc registers a callback invoked with short status strings
// ("Up to date", "Syncing…", "Paused", "Error") — used by the tray to update its tooltip.
func (e *Engine) SetStatusFunc(f func(string)) { e.onStatus = f }

func (e *Engine) status(s string) {
	e.diagMu.Lock()
	e.lastStatus = s
	if s == "Up to date" {
		e.lastSyncAt = time.Now()
	}
	e.diagMu.Unlock()
	if e.onStatus != nil {
		e.onStatus(s)
	}
}

// setPushState records the notify_push connection state for the health panel.
func (e *Engine) setPushState(up bool) {
	e.diagMu.Lock()
	if up && !e.pushUp {
		e.pushSince = time.Now()
	}
	e.pushUp = up
	e.diagMu.Unlock()
}

// Diagnostic is a snapshot of Nimbo's connection/sync health for the UI.
type Diagnostic struct {
	ServerURL     string
	ServerVersion string
	Account       string
	PushAvailable bool
	PushConnected bool
	PushSince     time.Time
	LastStatus    string
	LastSyncAt    time.Time
	// Local network route (spec 2026-09-13): "local"/"public", why public, and
	// the configured dial target ("" = none).
	Route        string
	RouteReason  string
	LocalAddress string
}

// Diagnostics returns a current health snapshot (no network calls).
func (e *Engine) Diagnostics() Diagnostic {
	e.diagMu.Lock()
	d := Diagnostic{
		PushConnected: e.pushUp,
		PushSince:     e.pushSince,
		LastStatus:    e.lastStatus,
		LastSyncAt:    e.lastSyncAt,
	}
	e.diagMu.Unlock()
	d.ServerURL = e.Account.ServerURL
	d.Account = e.Account.LoginName
	d.Route, d.RouteReason = e.client.Route()
	d.LocalAddress = e.client.LocalAddress()
	d.PushAvailable = e.PushAvailable()
	if e.caps != nil {
		d.ServerVersion = e.caps.Version.String
	}
	return d
}

// PauseSchedule is a daily quiet-hours window during which syncing auto-pauses.
// Times are minutes from midnight; an end before the start wraps past midnight.
type PauseSchedule struct {
	Enabled bool `json:"enabled"`
	FromMin int  `json:"fromMin"`
	ToMin   int  `json:"toMin"`
}

func (s PauseSchedule) activeAt(t time.Time) bool {
	if !s.Enabled || s.FromMin == s.ToMin {
		return false
	}
	m := t.Hour()*60 + t.Minute()
	if s.FromMin < s.ToMin {
		return m >= s.FromMin && m < s.ToMin
	}
	return m >= s.FromMin || m < s.ToMin // overnight window
}

func minToHHMM(m int) string { return fmt.Sprintf("%02d:%02d", (m/60)%24, m%60) }

// SetPaused pauses (indefinitely) or resumes syncing, clearing any timed pause.
func (e *Engine) SetPaused(p bool) {
	e.mu.Lock()
	e.paused = p
	e.pauseUntil = time.Time{}
	e.mu.Unlock()
	e.pauseChanged()
}

// PauseFor pauses syncing until now+d (d <= 0 pauses indefinitely).
func (e *Engine) PauseFor(d time.Duration) {
	e.mu.Lock()
	if d <= 0 {
		e.paused = true
		e.pauseUntil = time.Time{}
	} else {
		e.paused = false
		e.pauseUntil = time.Now().Add(d)
	}
	e.mu.Unlock()
	e.pauseChanged()
}

// SetPauseSchedule sets the quiet-hours auto-pause window.
func (e *Engine) SetPauseSchedule(s PauseSchedule) {
	e.mu.Lock()
	e.schedule = s
	e.mu.Unlock()
	e.pauseChanged()
}

// SetPauseChangeFunc registers a callback fired when the effective pause state
// changes (manual, timed expiry, or schedule boundary).
func (e *Engine) SetPauseChangeFunc(f func()) { e.onPauseChange = f }

// Paused reports whether syncing is currently paused (for any reason).
func (e *Engine) Paused() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.pausedLocked()
}

func (e *Engine) pausedLocked() bool {
	if e.paused {
		return true
	}
	if !e.pauseUntil.IsZero() && time.Now().Before(e.pauseUntil) {
		return true
	}
	return e.schedule.activeAt(time.Now())
}

// PauseStatus describes the effective pause state for the UI.
type PauseStatus struct {
	Paused bool   `json:"paused"`
	Reason string `json:"reason"` // manual | timed | scheduled | ""
	Until  string `json:"until"`  // HH:MM, when known
}

// PauseState returns the current effective pause state.
func (e *Engine) PauseState() PauseStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	switch {
	case e.paused:
		return PauseStatus{Paused: true, Reason: "manual"}
	case !e.pauseUntil.IsZero() && now.Before(e.pauseUntil):
		return PauseStatus{Paused: true, Reason: "timed", Until: e.pauseUntil.Format("15:04")}
	case e.schedule.activeAt(now):
		return PauseStatus{Paused: true, Reason: "scheduled", Until: minToHHMM(e.schedule.ToMin)}
	default:
		return PauseStatus{}
	}
}

// pauseChanged updates status, resumes work if newly unpaused, and notifies.
func (e *Engine) pauseChanged() {
	if e.Paused() {
		e.status("Paused")
	} else {
		e.status("Up to date")
		e.TriggerSync()
	}
	if e.onPauseChange != nil {
		e.onPauseChange()
	}
}

// TriggerSync requests an immediate sync of all watched pairs (used by the
// tray's "Sync now"). It is a no-op until Run has started.
func (e *Engine) TriggerSync() {
	e.watchMu.Lock()
	chans := make([]chan struct{}, 0, len(e.triggers))
	for _, ch := range e.triggers {
		chans = append(chans, ch)
	}
	e.watchMu.Unlock()
	for _, ch := range chans {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// TriggerFullSync forces every pair to do a FULL local-walking reconcile now,
// rather than the fast remote-delta a plain TriggerSync fires. Needed after a
// change to the name rules (allow-list / escape-list) that reclassifies LOCAL
// files — a remote-delta wouldn't re-examine them. No-op until Run has started.
func (e *Engine) TriggerFullSync() {
	e.watchMu.Lock()
	chans := make([]chan struct{}, 0, len(e.triggersFull))
	for _, ch := range e.triggersFull {
		chans = append(chans, ch)
	}
	e.watchMu.Unlock()
	for _, ch := range chans {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// PairKey derives the baseline identity for a pair (see state.pair_key).
func PairKey(localDir, remoteRoot string) string {
	abs, err := filepath.Abs(localDir)
	if err != nil {
		abs = localDir
	}
	abs = strings.ToLower(filepath.Clean(abs))
	sum := sha256.Sum256([]byte(abs + "|" + remoteRoot))
	return hex.EncodeToString(sum[:8])
}

// ensurePair makes sure the local directory and remote root exist.
func (e *Engine) ensurePair(ctx context.Context, p Pair) error {
	// Refresh the guard state here rather than only at start-up. This is the
	// one call every sync entry point makes before computing a plan, so a
	// folder frozen by another pass moments ago cannot slip through on a stale
	// set. It costs the same small file read applyPlan already does for the
	// blacklist. Failing here stops the pass before the scan rather than after
	// it; applyPlan re-checks anyway, for callers that reach it another way.
	if err := e.reloadGuardState(); err != nil {
		return err
	}
	// A frozen folder does nothing at all until the user has looked at it. This
	// is upstream of the clone, which never reaches applyPlan's own check — and
	// a state-database reset is exactly what routes a frozen pair back into the
	// clone, where a re-download would overwrite what the freeze was protecting.
	if err := e.frozenPairErr(p); err != nil {
		return err
	}
	if err := os.MkdirAll(p.LocalDir, 0o755); err != nil {
		return err
	}
	// Create the remote root once per run, not once per pass: this sits on the
	// path of EVERY sync entry point, and re-ensuring cost one MKCOL per path
	// segment every few minutes, forever (visible in a server's access log as
	// an endless MKCOL drumbeat — #599). If the root later vanishes
	// server-side the scan fails loudly, which is the safer outcome anyway.
	if p.RemoteRoot != "" {
		e.ensuredMu.Lock()
		done := e.ensuredRoots[p.RemoteRoot]
		e.ensuredMu.Unlock()
		if !done {
			if err := e.client.EnsureCollection(ctx, p.RemoteRoot); err != nil {
				return fmt.Errorf("ensure remote root: %w", err)
			}
			e.ensuredMu.Lock()
			if e.ensuredRoots == nil {
				e.ensuredRoots = map[string]bool{}
			}
			e.ensuredRoots[p.RemoteRoot] = true
			e.ensuredMu.Unlock()
		}
	}
	return nil
}

// computePlan scans both sides, diffs against the baseline, and coalesces renames.
func (e *Engine) computePlan(ctx context.Context, st *state.Store, p Pair) ([]engine.Action, map[string]engine.RemoteState, map[string]engine.BaselineState, error) {
	pk := PairKey(p.LocalDir, p.RemoteRoot)
	// Narrate the stages. On a big change batch the scan alone runs for minutes,
	// and one flat "Scanning…" for all of it is indistinguishable from a hang.
	// Nothing is emitted until the pass outlives scanQuietPeriod, so the warm
	// 15-second poll stays silent (see scanstatus.go).
	e.scanBegin()
	e.scanPhase("Reading your file list…")
	base, err := st.LoadBaseline(pk)
	if err != nil {
		return nil, nil, nil, err
	}
	// Exclude ignored paths from both sides so they're left untouched (not synced,
	// not deleted): global patterns + this pair's own excludes. Passing ig.Match to
	// RemoteScan also prunes the PROPFIND descent so ignored trees (node_modules,
	// .git, …) don't hammer the server; FilterRemote then stays as a safety net.
	ig := e.ignoreFor(p)
	cp := newScanCheckpoint(st, pk)
	e.scanPhase("Checking server…")
	remote, err := engine.RemoteScan(ctx, e.client, p.RemoteRoot, engine.ScanOpts{
		Base: base, Skip: ig.Match, OnEncrypted: e.noteEncrypted, Esc: e.escaper.Load(),
		Checkpoint: cp,
		Progress: func(dirs int) {
			e.scanTick("Checking server… " + commas(dirs) + " folders")
		},
	})
	cp.logSummary()
	if _, _, saves := cp.stats(); saves > 0 {
		e.markCheckpointDirty(pk)
	}
	if err != nil {
		return nil, nil, nil, fmt.Errorf("remote scan: %w", err)
	}
	e.scanPhase("Checking your files…")
	local, err := engine.LocalScanProgress(p.LocalDir, "", func(files int) {
		e.scanTick("Checking your files… " + commas(files))
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("local scan: %w", err)
	}
	ig.FilterLocal(local)
	ig.FilterRemote(remote)

	e.scanPhase("Comparing changes…")
	actions := engine.Diff(base, remote, local)
	pruneDeadBaselines(st, pk, base, remote, local)

	// Rename coalescing SHA1-hashes local files to match them against the
	// baseline — the slowest stage of all on a big batch of new files, and until
	// now completely invisible. CoalesceRenames drives hashLocal from its own
	// sequential loop, so a plain counter is safe here.
	e.scanPhase("Matching moved files…")
	hashed := 0
	hashLocal := func(rel string) (string, error) {
		hashed++
		e.scanTick("Matching moved files… " + commas(hashed))
		return transfer.SHA1File(filepath.Join(p.LocalDir, filepath.FromSlash(rel)))
	}
	return engine.CoalesceRenames(actions, base, remote, local, hashLocal), remote, base, nil
}

// computePlanScoped is computePlan limited to one subtree (scope, a pair-relative
// directory). It scans only that branch on both sides — the remote scan is rooted
// at the subtree, the local walk only descends it, and the baseline is loaded for
// just that subtree — then keeps everything keyed pair-relative so the resulting
// plan applies through the normal pair Executor. scope == "" yields a full plan.
//
// Moves that cross the scope boundary can't be detected within a single scoped
// pass (they look like a delete on one side); the periodic full poll reconciles
// those.
func (e *Engine) computePlanScoped(ctx context.Context, st *state.Store, p Pair, scope string) ([]engine.Action, map[string]engine.RemoteState, map[string]engine.BaselineState, error) {
	scope = strings.Trim(scope, "/")
	if scope == "" {
		return e.computePlan(ctx, st, p)
	}
	base, err := st.LoadBaselineScoped(PairKey(p.LocalDir, p.RemoteRoot), scope)
	if err != nil {
		return nil, nil, nil, err
	}
	ig := e.ignoreFor(p)
	// RemoteScan keys relative to its root, so root it at the subtree (re-keying the
	// baseline down) and lift the result back to pair-relative keys. The scan keys
	// are scope-relative, but ig's patterns are pair-relative, so prefix the scope
	// when consulting the ignore matcher to prune the descent.
	subRoot := strings.Trim(p.RemoteRoot+"/"+scope, "/")
	subRemote, err := engine.RemoteScan(ctx, e.client, subRoot, engine.ScanOpts{
		Base:        stripScopePrefix(base, scope),
		Skip:        func(rel string) bool { return ig.Match(scope + "/" + rel) },
		OnEncrypted: e.noteEncrypted,
		Esc:         e.escaper.Load(),
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("remote scan: %w", err)
	}
	remote := addScopePrefix(subRemote, scope)
	local, err := engine.LocalScanScoped(p.LocalDir, scope)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("local scan: %w", err)
	}
	ig.FilterLocal(local)
	ig.FilterRemote(remote)

	actions := engine.Diff(base, remote, local)
	pruneDeadBaselines(st, PairKey(p.LocalDir, p.RemoteRoot), base, remote, local)
	hashLocal := func(rel string) (string, error) {
		return transfer.SHA1File(filepath.Join(p.LocalDir, filepath.FromSlash(rel)))
	}
	return engine.CoalesceRenames(actions, base, remote, local, hashLocal), remote, base, nil
}

// stripScopePrefix re-keys a pair-relative baseline to scope-relative (drops the
// "scope/" prefix), for feeding RemoteScan rooted at the subtree.
func stripScopePrefix(base map[string]engine.BaselineState, scope string) map[string]engine.BaselineState {
	pre := scope + "/"
	out := make(map[string]engine.BaselineState, len(base))
	for k, b := range base {
		nk, ok := strings.CutPrefix(k, pre)
		if !ok {
			continue // not under scope (a scoped load shouldn't contain these)
		}
		b.Path = nk
		out[nk] = b
	}
	return out
}

// addScopePrefix lifts a subtree-relative remote map back to pair-relative keys.
func addScopePrefix(remote map[string]engine.RemoteState, scope string) map[string]engine.RemoteState {
	out := make(map[string]engine.RemoteState, len(remote))
	for k, r := range remote {
		nk := scope + "/" + k
		r.Path = nk
		out[nk] = r
	}
	return out
}

// getStore returns the engine's resident state store, opening it (and its baseline
// cache) on first use. The handle lives for the engine's lifetime so the cache
// stays warm across syncs; callers must not Close it — use closeStore on shutdown.
func (e *Engine) getStore() (*state.Store, error) {
	e.storeMu.Lock()
	defer e.storeMu.Unlock()
	if e.storeFinal {
		return nil, errors.New("engine stopped — state store closed")
	}
	if e.store != nil {
		return e.store, nil
	}
	cacheBaseline := false // default: low-memory mode (read baseline from disk)
	if s, err := e.dirs.LoadSettings(); err == nil {
		cacheBaseline = s.KeepBaselineInMemory
	}
	st, err := state.Open(e.dirs.StateDB(e.Account.ID), e.Account.ID, cacheBaseline)
	if err != nil {
		return nil, err
	}
	// Opportunistic age-out: checkpoint rows from crawls that never reached a
	// clean pass expire after 14 days. Runs once per store open (≈ engine
	// lifetime; also covers CLI one-shots).
	if err := st.DeleteScanCheckpointBefore(time.Now().Add(-14 * 24 * time.Hour)); err != nil {
		slog.Warn("scan checkpoint age-out failed", "err", err)
	}
	e.store = st
	return st, nil
}

// ReloadStore drops the resident state store so the next sync reopens it with the
// current settings — used after toggling the in-memory baseline ("low memory
// mode"). Frees the dropped cache promptly.
func (e *Engine) ReloadStore() {
	e.closeStore()
	e.releaseHeap()
}

// releaseHeap returns the memory a full reconcile allocated (the transient remote
// and local scan maps) to the OS promptly, so the process doesn't sit at the
// sync's high-water mark between syncs. It's a STW GC + scavenge — cheap relative
// to a sync, and keeps the idle footprint low.
func (e *Engine) releaseHeap() { debug.FreeOSMemory() }

// drainWatchers waits (bounded overall) for every watcher goroutine — and
// hence any in-flight sync pass — to exit. Watchers stop promptly once their
// contexts are cancelled; the bound keeps one wedged sync from hanging
// shutdown forever (the stopWatcherSync convention).
func (e *Engine) drainWatchers(timeout time.Duration) {
	e.watchMu.Lock()
	dones := make([]chan struct{}, 0, len(e.watchDone))
	for _, d := range e.watchDone {
		dones = append(dones, d)
	}
	e.watchMu.Unlock()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for _, d := range dones {
		select {
		case <-d:
		case <-deadline.C:
			slog.Warn("watchers did not drain in time; closing the state store anyway", "timeout", timeout)
			return
		}
	}
}

// closeStoreFinal closes the resident store and refuses future opens — after
// Run exits nothing owns the handle, so a straggling sync pass lazily
// reopening it would leak the DB until process exit.
func (e *Engine) closeStoreFinal() {
	e.storeMu.Lock()
	e.storeFinal = true
	e.storeMu.Unlock()
	e.closeStore()
}

// closeStore closes the resident store if open. Called when Run exits.
func (e *Engine) closeStore() {
	e.storeMu.Lock()
	defer e.storeMu.Unlock()
	if e.store != nil {
		_ = e.store.Close()
		e.store = nil
	}
}

// Plan returns the reconciliation actions for a pair without applying them.
func (e *Engine) Plan(ctx context.Context, p Pair) ([]engine.Action, error) {
	if err := e.ensurePair(ctx, p); err != nil {
		return nil, err
	}
	st, err := e.getStore()
	if err != nil {
		return nil, err
	}
	actions, _, _, err := e.computePlan(ctx, st, p)
	return actions, err
}

// SyncOnce reconciles a pair and applies the plan. It is a no-op while paused.
func (e *Engine) SyncOnce(ctx context.Context, p Pair) (transfer.Stats, error) {
	if e.Paused() {
		return transfer.Stats{}, nil
	}
	// Move/sync mutual exclusion: skip this pass entirely while a folder move
	// holds the lock — the post-move confirming sync reconciles afterwards.
	if !e.beginSyncPass() {
		return transfer.Stats{}, nil
	}
	defer e.endSyncPass()
	if ex, ok := e.excludesFor(p.LocalDir, p.RemoteRoot); ok {
		p.Excludes = ex // watcher captured p at start; pick up selective-sync toggles now
	}
	if err := e.ensurePair(ctx, p); err != nil {
		return transfer.Stats{}, err
	}
	defer e.releaseHeap() // a full reconcile builds big transient maps — hand them back
	st, err := e.getStore()
	if err != nil {
		return transfer.Stats{}, err
	}

	// Initial sync uses a resumable bulk clone (pure download — nothing to delete
	// or move); the diff path takes over once it's fully done. A pre-existing
	// synced pair (baseline but no clone state) is marked done so it isn't recloned.
	pk := PairKey(p.LocalDir, p.RemoteRoot)
	// A clone walks the REMOTE tree only, so it is blind to local-only content:
	// pairing an already-populated folder with an empty remote used to produce
	// zero actions, mark the clone done and report "Up to date" having uploaded
	// nothing. Sampled BEFORE the clone runs, since the clone itself populates
	// the folder. When there is local content the pass continues into the diff
	// below, which classifies local-only paths as uploads.
	localHadContent := !localRootVanished(p.LocalDir)
	var cloneStats transfer.Stats
	switch status, _ := st.CloneStatus(pk); {
	case status == "done":
		// Proceed to the diff path below. Backfill the config-side sync-history
		// marker: pairs that finished their clone before the marker existed must
		// still be covered by the state-reset tripwire.
		e.markPairSyncedOnce(pk)
	case status == "started":
		e.status("Syncing…")
		s, err := e.cloneRemote(ctx, st, p)
		if err != nil || !localHadContent {
			return s, err
		}
		cloneStats = s
	default:
		if empty, _ := st.BaselineEmpty(pk); empty {
			e.warnIfStateReset(pk, p) // synced before but no state? say so before the takeover
			e.status("Syncing…")
			s, err := e.cloneRemote(ctx, st, p)
			if err != nil || !localHadContent {
				return s, err
			}
			cloneStats = s
			break // clone done; fall through to reconcile the local-only side
		}
		_ = st.SetCloneStatus(pk, "done")
		e.markPairSynced(pk) // baseline present = synced before; seed the config-side marker
	}

	// A full pass scans both trees before any transfer; on a large sync the
	// discovery alone takes minutes, so surface it rather than looking idle.
	e.status("Scanning…")
	actions, remote, base, err := e.computePlan(ctx, st, p)
	if err != nil {
		// A failed scan must not leave the flyout stuck on "Scanning…". Classify it
		// the way applyPlan does so the status reflects reality (skip on shutdown /
		// watcher-restart cancellation, which isn't a real error).
		if ctx.Err() == nil {
			switch syncErrKind(err) {
			case "auth":
				e.status("Sign in again")
				e.authLost()
			case "offline":
				e.status("Offline")
			default:
				e.status("Error")
			}
		}
		return transfer.Stats{}, err
	}
	planStats, err := e.applyPlan(ctx, st, p, actions, remote, base, true) // full reconcile
	// cloneStats is non-zero only when this pass also ran a clone first, so the
	// caller sees one set of totals for the whole pass.
	return cloneStats.Plus(planStats), err
}

// cloneEnumConcurrency bounds concurrent recursive PROPFINDs while the tree is
// being counted; cloneDownloadConcurrency bounds how many top-level folders
// download at once (each via its own Executor worker pool). Enumeration runs
// ahead of (and independently of) downloading so the progress total is known in
// minutes instead of being chained to the slow downloads.
const (
	cloneEnumConcurrency     = 6
	cloneDownloadConcurrency = 4
)

// cloneRemote performs a resumable initial sync. It enumerates the tree in bulk
// — one recursive PROPFIND per top-level folder instead of one request per
// directory, so a huge account lists in a couple of minutes rather than ~half an
// hour — and downloads through the normal Executor, so baselines are recorded
// exactly as a regular sync would. Files already present locally (matching size)
// are skipped, so an interrupted clone resumes without re-downloading. It is
// marked "done" only once the whole tree is cloned; until then SyncOnce re-enters
// here instead of using the diff path, so a partial clone is never mistaken for a
// fully-synced pair.
func (e *Engine) cloneRemote(ctx context.Context, st *state.Store, p Pair) (transfer.Stats, error) {
	pk := PairKey(p.LocalDir, p.RemoteRoot)
	// Takeover: a first-ever clone (no prior status) into a folder that ALREADY has
	// files — e.g. migrating from the official Nextcloud client. We adopt files that
	// match the server (size+mtime) instead of re-downloading, and crucially never
	// overwrite a local file that differs: it's left untouched with no baseline, so
	// the first normal sync surfaces it as a conflict (keeps both). A resume (status
	// already "started") is NOT a takeover — a wrong-size file there is a partial
	// download to refetch.
	priorStatus, _ := st.CloneStatus(pk)
	_ = st.SetCloneStatus(pk, "started")
	// Entering (or resuming) a clone: any checkpoint rows are from a pre-clone
	// life of this pair — there is no baseline worth chaining against, so drop
	// them rather than let stale rescue rows linger for the age-out.
	e.clearCheckpoint(st, pk)
	if err := os.MkdirAll(p.LocalDir, 0o755); err != nil {
		return transfer.Stats{}, err
	}
	takeover := priorStatus == "" && !localRootVanished(p.LocalDir)
	if takeover {
		slog.Info("takeover: adopting matching local files, conflicting the rest", "local", p.LocalDir)
	}
	e.progStart(0, 0) // totals grow as folders are enumerated (progAddTotal)
	defer e.progEnd()
	e.progMu.Lock()
	e.prog.Enumerating = true // indeterminate until the whole tree is listed
	e.progMu.Unlock()
	root := strings.Trim(p.RemoteRoot, "/")
	ig := e.ignoreFor(p)

	esc := e.escaper.Load()
	relOf := func(full string) string {
		rel := full
		if root != "" {
			rel = strings.TrimPrefix(full, root+"/")
		}
		// An escaped server name (X<suffix>) maps to its local decoded name X, so
		// the clone downloads/adopts/baselines it under the name it lives as
		// locally; the Executor re-encodes for the actual GET.
		rel, _ = esc.Decode(rel)
		return rel
	}
	newExec := func(remote map[string]engine.RemoteState) *transfer.Executor {
		return &transfer.Executor{
			Client: e.client, State: st, PairKey: pk,
			LocalRoot: p.LocalDir, RemoteRoot: p.RemoteRoot, Remote: remote, Escaper: e.escaper.Load(),
			Workers: 8, Policy: e.policy, // ×cloneDownloadConcurrency = total in-flight (32). 64 gave no gain — the path caps ~5 MB/s, not the client.
			OnBegin: func(a engine.Action) {
				e.markInflight(filepath.Join(p.LocalDir, filepath.FromSlash(a.Path)), true)
				if a.Kind == engine.ActDownload {
					e.progCurrent(filepath.Base(a.Path))
				}
			},
			OnProgress: func(a engine.Action, d int64) { e.progBytes.Add(d) },
			OnEvent: func(a engine.Action, aerr error) {
				e.markInflight(filepath.Join(p.LocalDir, filepath.FromSlash(a.Path)), false)
				if a.Kind == engine.ActDownload {
					e.progComplete()
				}
				var damaged *transfer.ChecksumMismatchError
				if a.Kind == engine.ActDownload && errors.As(aerr, &damaged) {
					e.noteDamaged(pk, a.Path, remote[a.Path].ETag) // see damaged.go
				}
				ev := activity.Event{Local: p.LocalDir, Path: a.Path, Kind: a.Kind.String()}
				ev.Err = e.recordActionResult(a, aerr) // humanised + deduped; "" on success
				e.recorder.Add(ev)
			},
		}
	}

	var (
		mu     sync.Mutex
		stats  transfer.Stats
		failed error
	)
	note := func(s transfer.Stats, err error) {
		mu.Lock()
		stats = stats.Plus(s)
		if err != nil && failed == nil {
			failed = err
		}
		mu.Unlock()
	}

	// plan turns pair-relative remote entries into mkdir + download actions
	// (skipping files already on disk) and returns the download file count + bytes
	// so the caller can grow the progress total. It filters `remote` in place.
	plan := func(remote map[string]engine.RemoteState) (actions []engine.Action, dlFiles int, dlBytes int64) {
		ig.FilterRemote(remote)
		for rel, r := range remote {
			if r.IsDir {
				actions = append(actions, engine.Action{Kind: engine.ActCreateLocalDir, Path: rel})
				continue
			}
			var fi os.FileInfo
			if info, serr := os.Stat(filepath.Join(p.LocalDir, filepath.FromSlash(rel))); serr == nil {
				fi = info
			}
			switch decideCloneFile(takeover, fi, cfapi.IsDehydrated(fi), r) {
			case cloneAdopt:
				_ = st.UpsertBaseline(pk, baselineForLocal(p.LocalDir, rel, r))
			case cloneDownload:
				actions = append(actions, engine.Action{Kind: engine.ActDownload, Path: rel})
				dlFiles++
				dlBytes += r.Size
			case cloneSkip:
				// Takeover: differs from the server — leave it untouched with no
				// baseline; the first normal sync surfaces it as a conflict (keep both).
			}
		}
		return
	}

	// Root: one Depth:1 listing for the account-root files + the top-level folders.
	rootEntries, err := e.client.PropFind(ctx, root, 1)
	if err != nil {
		return stats, err
	}
	rootRemote := make(map[string]engine.RemoteState)
	var topDirs []string
	rootOnMount, rootKnown := listingSelf(rootEntries, root)
	for _, en := range rootEntries {
		full := strings.Trim(en.Path, "/")
		if full == root {
			continue
		}
		rel := relOf(full)
		rootRemote[rel] = cloneRemoteState(rel, en, rootOnMount, rootKnown)
		if en.IsDir {
			topDirs = append(topDirs, full)
		}
	}
	rootActions, rootFiles, rootBytes := plan(rootRemote) // filters rootRemote in place

	// Folders that survived the ignore filter get recursed.
	var dispatch []string
	for _, td := range topDirs {
		if _, kept := rootRemote[relOf(td)]; kept {
			dispatch = append(dispatch, td)
		}
	}

	// "Enumerating" stays true until every folder's listing has been counted, so
	// the bar is indeterminate (not misleadingly near-full) while the total grows.
	enumPending := int32(1 + len(dispatch)) // root + each folder
	enumDone := func() {
		if atomic.AddInt32(&enumPending, -1) == 0 {
			e.progMu.Lock()
			e.prog.Enumerating = false
			e.progMu.Unlock()
			e.emitProgress()
		}
	}

	// Root contents first (its folders are created here), then count it.
	e.progAddTotal(rootFiles, rootBytes)
	enumDone()
	if len(rootActions) > 0 {
		s, rerr := newExec(rootRemote).Run(ctx, rootActions)
		note(s, rerr)
	}

	// Counting must not wait on the slow downloads, or "enumerating" drags on for
	// the whole clone (the total isn't known until the last folder is listed). So
	// list every folder up-front — bounded only by PROPFIND concurrency — and count
	// it immediately; that clears "enumerating" within minutes. Each listing is
	// then handed to a separate download pool and freed once its folder completes,
	// so peak memory falls as the clone progresses.
	type folderWork struct {
		sub     map[string]engine.RemoteState
		actions []engine.Action
	}
	// Buffered to len(dispatch) so a just-counted folder can always be queued
	// without blocking enumeration behind a download that's still catching up.
	works := make(chan folderWork, len(dispatch))

	var dlwg sync.WaitGroup
	for i := 0; i < cloneDownloadConcurrency; i++ {
		dlwg.Add(1)
		go func() {
			defer dlwg.Done()
			for w := range works {
				s, rerr := newExec(w.sub).Run(ctx, w.actions)
				note(s, rerr)
			}
		}()
	}

	sem := make(chan struct{}, cloneEnumConcurrency)
	var enwg sync.WaitGroup
	for _, td := range dispatch {
		enwg.Add(1)
		sem <- struct{}{}
		go func(dirFull string) {
			defer enwg.Done()
			defer func() { <-sem }()
			entries, perr := e.client.PropFindRecursive(ctx, dirFull)
			if perr != nil {
				note(transfer.Stats{}, perr)
				enumDone()
				return
			}
			sub := make(map[string]engine.RemoteState, len(entries))
			// Every ancestor of an entry is in the same recursive listing, except
			// the top folder's own parent — the pair root, read above.
			onMount := make(map[string]bool, len(entries))
			for _, en := range entries {
				onMount[strings.Trim(en.Path, "/")] = en.OnMount()
			}
			for _, en := range entries {
				full := strings.Trim(en.Path, "/")
				rel := relOf(full)
				if rel == "" {
					continue
				}
				parentOnMount, parentKnown := rootOnMount, rootKnown
				if parent := dirParent(full); parent != root {
					parentOnMount, parentKnown = onMount[parent]
				}
				sub[rel] = cloneRemoteState(rel, en, parentOnMount, parentKnown)
			}
			actions, dlF, dlB := plan(sub)
			e.progAddTotal(dlF, dlB)
			enumDone()
			if len(actions) > 0 {
				works <- folderWork{sub: sub, actions: actions}
			}
		}(td)
	}
	enwg.Wait()  // every folder listed + counted → "enumerating" is now false
	close(works) // no more folders to hand to the download pool
	dlwg.Wait()  // wait for the download pool to drain

	if failed != nil {
		e.status("Error")
		return stats, failed
	}
	if ctx.Err() != nil {
		return stats, ctx.Err()
	}
	_ = st.SetCloneStatus(pk, "done")
	e.markPairSynced(pk)
	e.status("Up to date")
	return stats, nil
}

// localRootVanished reports whether a pair's local root is missing, unreadable, or
// empty — the signature of a sync folder that was deleted, moved, or not mounted.
// The data-loss guard in applyPlan uses it to refuse propagating deletions to the
// server in that state. Reads only the root's immediate entries (cheap).
func localRootVanished(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return true // missing or unreadable — treat as vanished, never delete remotely
	}
	return len(entries) == 0
}

// Data-loss circuit-breaker thresholds. A single sync pass that would delete at
// least guardDeleteFloor of a pair's files AND at least guardDeletePct percent of
// them is treated as a vanished/moved folder (or a bug), not an intentional bulk
// delete, and refused. Below the floor, or a small fraction, deletions pass.
const (
	guardDeleteFloor = 50
	guardDeletePct   = 50
)

// bulkDeleteGuardTrips reports whether deleting `deletes` of a pair's `total` known
// files in one pass looks like a suspicious bulk deletion the guard should refuse.
func bulkDeleteGuardTrips(deletes, total int) bool {
	return deletes >= guardDeleteFloor && total >= guardDeleteFloor && deletes*100 >= total*guardDeletePct
}

// deletionWeight is how many known rows a plan's deletions of one kind remove:
// each deleted path, plus every row beneath a deleted directory. Deleting a
// directory is ONE action however much it holds, so counting actions let a
// scoped pass delete a 180,810-file folder as "1", under both guards (Deck
// #691). A path beneath another deleted path is already covered and is not
// counted twice, which keeps a full scan (one action per file) at its old weight.
// Files the same plan MOVES out first are not deleted either: rename coalescing
// pairs files, so a folder rename is per-file moves plus the old folder's delete.
func deletionWeight(st *state.Store, pk string, actions []engine.Action, kind engine.ActionKind) (int, error) {
	move := engine.ActMoveLocal
	if kind == engine.ActDeleteRemote {
		move = engine.ActMoveRemote
	}
	deleted := make(map[string]struct{})
	for _, a := range actions {
		if a.Kind == kind {
			deleted[a.Path] = struct{}{}
		}
	}
	beneathDeleted := func(p string) bool {
		for d := dirParent(p); d != ""; d = dirParent(d) {
			if _, ok := deleted[d]; ok {
				return true
			}
		}
		return false
	}
	var top []string
	for p := range deleted {
		if !beneathDeleted(p) {
			top = append(top, p)
		}
	}
	if len(top) == 0 {
		return 0, nil
	}
	beneath, err := st.BaselineCountUnder(pk, top)
	if err != nil {
		return 0, err
	}
	for _, a := range actions {
		if a.Kind == move && beneathDeleted(a.Path) {
			beneath--
		}
	}
	return len(top) + beneath, nil
}

// humanActionErr turns a raw transfer/WebDAV error into a short, human-readable
// reason for the log and the activity feed. The .Collectives case is the common
// one: the Collectives app rejects directory creation there (a quirky 507), so
// anything a user drops into that folder can't be uploaded.
func humanActionErr(a engine.Action, err error) string {
	s := err.Error()
	low := strings.ToLower(s)
	switch {
	case strings.HasPrefix(a.Path, ".Collectives") || strings.Contains(s, ".Collectives"):
		return "can't sync inside “.Collectives” — it's managed by the Collectives app and won't accept items created here; move this out of .Collectives to sync it"
	case transport.IsLocked(err):
		return "someone else has this file open — it's locked on the server"
	case errors.As(err, new(*transfer.ChecksumMismatchError)):
		return damagedCopyMsg
	case strings.Contains(low, "insufficientstorage") || strings.Contains(s, "507"):
		return "the server wouldn't accept it (out of space, or the folder is read-only)"
	case strings.Contains(low, "parent node does not exist") || strings.Contains(s, "409"):
		return "its parent folder couldn't be created on the server"
	case strings.Contains(low, "forbidden") || strings.Contains(s, "403"):
		return "the server refused it (permission denied)"
	case strings.Contains(low, "access is denied"):
		return "Windows denied access to that path (it may be read-only or locked)"
	case strings.Contains(low, "unauthorized") || strings.Contains(s, "401"):
		return "the server rejected our sign-in — you may need to log in again"
	default:
		return s
	}
}

// recordActionResult logs a failed action ONCE (deduped per path+reason, with a
// human-readable message) and returns that message for the activity feed; a later
// success for the same path clears the record so a fresh failure logs again.
// Returns "" when there's no error. This is what stops a permanently-rejected
// item (e.g. something under .Collectives) from spamming the log every sync pass.
func (e *Engine) recordActionResult(a engine.Action, aerr error) string {
	key := a.Kind.String() + "\x00" + a.Path
	e.failMu.Lock()
	if e.lastFail == nil {
		e.lastFail = make(map[string]string)
	}
	if aerr == nil {
		delete(e.lastFail, key)
		e.failMu.Unlock()
		return ""
	}
	human := humanActionErr(a, aerr)
	prev, seen := e.lastFail[key]
	e.lastFail[key] = human
	e.failMu.Unlock()
	if !seen || prev != human {
		slog.Warn("couldn't sync an item", "path", a.Path, "op", a.Kind.String(), "reason", human)
	}
	return human
}

// cloneDecision is what a clone does with one remote file given the local state.
type cloneDecision int

const (
	cloneDownload cloneDecision = iota // fetch from the server
	cloneAdopt                         // local already matches — record baseline, no transfer
	cloneSkip                          // takeover: local differs — leave it (a later sync conflicts it)
)

// decideCloneFile decides what to do with a remote file during a clone. localFI is
// the local file's stat, or nil if absent. On a resume (not takeover) a size match
// means the file is already downloaded; a mismatch is a partial download to refetch.
// On a takeover it adopts only an exact match (size + mtime within 2s, since the
// official client preserves server mtimes) and never overwrites — a differing file
// is skipped so the first normal sync keeps both versions.
//
// dehydrated marks a local file whose bytes are not actually on disk (a cloud
// placeholder — see cfapi.IsDehydrated), which is always fetched instead.
func decideCloneFile(takeover bool, localFI os.FileInfo, dehydrated bool, r engine.RemoteState) cloneDecision {
	if localFI == nil || localFI.IsDir() {
		return cloneDownload
	}
	// A placeholder reports the server's size and mtime while holding no content,
	// so every comparison below would wrongly read it as "already present". Left
	// adopted, it is recorded as synced and never repaired — and once the client
	// that created it is uninstalled, nothing can hydrate it, so the user is left
	// with files that cannot be opened. Fetching loses nothing: a file with no
	// bytes on disk cannot hold local edits (writing to one hydrates it first).
	if dehydrated {
		return cloneDownload
	}
	if !takeover {
		if localFI.Size() == r.Size {
			return cloneAdopt
		}
		return cloneDownload
	}
	if engine.LocalMatchesRemote(localFI, r) {
		return cloneAdopt
	}
	return cloneSkip
}

// baselineForLocal builds a baseline row for a file already present locally
// (size/mtime from disk, etag/fileid from the server) so a resumed clone keeps it
// out of future re-downloads. ContentSHA1 is left empty; a later sync fills it.
func baselineForLocal(localRoot, rel string, r engine.RemoteState) engine.BaselineState {
	b := engine.BaselineState{Path: rel, RemoteETag: r.ETag, RemoteFileID: r.FileID, LocalSize: r.Size, MountRoot: r.MountRoot}
	if fi, err := os.Stat(filepath.Join(localRoot, filepath.FromSlash(rel))); err == nil {
		b.LocalSize = fi.Size()
		b.LocalMTimeNanos = fi.ModTime().UnixNano()
	}
	return b
}

// pruneDeadBaselines deletes baseline rows for paths present on neither side.
// The diff classifies those as noop with the promise the row "will be pruned
// during execution" — this is that prune (noops are dropped from the plan, so
// nothing else ever did it). Without it, dead rows whose listed ancestor is
// re-scanned surface as changed paths on every pass, keeping the remote-delta
// off its no-change fast path forever.
func pruneDeadBaselines(st *state.Store, pk string, base map[string]engine.BaselineState, remote map[string]engine.RemoteState, local map[string]engine.LocalState) {
	dead := engine.DeadBaselinePaths(base, remote, local)
	if len(dead) == 0 {
		return
	}
	if err := st.DeleteBaselineBatch(pk, dead); err != nil {
		slog.Warn("dead-baseline prune failed", "err", err)
		return
	}
	slog.Info("baseline pruned", "dead_rows", len(dead))
}

// dirParent returns the parent directory of a "/"-separated pair-relative path,
// or "" at the pair root.
func dirParent(rel string) string {
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		return rel[:i]
	}
	return ""
}

// maintainDirBaselines keeps the remote scan's ETag prune effective. The prune
// skips a directory only while its baseline etag equals the server's, but dir
// rows were historically written just once (at creation) — and Nextcloud bumps
// every ancestor's etag on any change — so each scan re-listed every directory
// that had EVER changed since its row was written: an ever-growing set (observed
// in the field: ~6,000 PROPFINDs ≈ 3 minutes per delta on a 95k-dir tree).
//
// After a pass, stamp the scan-time etag of every re-listed directory whose
// subtree fully reconciled, and dirty (empty etag) the existing rows on the
// ancestor chains of every path that failed or conflicted — a stamped ancestor
// must never hide unfinished work (e.g. a freshly created dir whose child
// download failed would otherwise be pruned over and the child never retried).
// Stamping the SCAN-time etag is
// TOCTOU-safe: anything the server changed after the scan carries a newer etag,
// so it still fails the prune next pass.
//
// remote must come from a real subtree-listing scan (full/scoped/delta): its dir
// entries carry scan-time etags and imply the subtree was walked. Pass base=nil
// when it doesn't (SyncPaths' stat-built map) — then only dirtying runs.
func maintainDirBaselines(st *state.Store, pk string, base map[string]engine.BaselineState, remote map[string]engine.RemoteState, problems []string) {
	dirty := make(map[string]struct{})
	failed := make(map[string]struct{}, len(problems))
	for _, pth := range problems {
		failed[pth] = struct{}{}
		for d := dirParent(pth); d != ""; d = dirParent(d) {
			dirty[d] = struct{}{}
		}
	}
	var rows []engine.BaselineState
	if base != nil {
		for pth, r := range remote {
			if !r.IsDir || pth == "" {
				continue
			}
			if _, poisoned := dirty[pth]; poisoned {
				continue
			}
			if _, bad := failed[pth]; bad {
				continue // its own action failed (e.g. the local mkdir): not reconciled
			}
			if b, ok := base[pth]; ok && b.IsDir && b.RemoteETag == r.ETag {
				continue // still fresh — was pruned or genuinely unchanged
			}
			rows = append(rows, engine.BaselineState{Path: pth, IsDir: true, RemoteETag: r.ETag, RemoteFileID: r.FileID, MountRoot: r.MountRoot})
		}
	}
	healed := len(rows)
	// Dirtying REWRITES a row that exists now, after this pass's actions; it
	// never creates one. A row says "synced on both sides", so one written for a
	// folder this pass deleted, or never managed to create, reads next pass as a
	// local deletion and is sent to the server (Deck #691). That is why the rows
	// are read back from the store: base predates the pass and still holds any
	// row the pass deleted. The same rows carry the share/mount-root flag over,
	// or the next unshare of that folder deletes it.
	var existing map[string]engine.BaselineState
	if len(dirty) > 0 {
		paths := make([]string, 0, len(dirty))
		for d := range dirty {
			paths = append(paths, d)
		}
		var err error
		if existing, err = st.LoadBaselinePaths(pk, paths); err != nil {
			slog.Warn("dir-baseline maintenance: cannot read rows to dirty", "err", err)
		}
	}
	dirtied := 0
	for d := range dirty {
		b, ok := existing[d]
		if !ok {
			continue
		}
		row := engine.BaselineState{Path: d, IsDir: true, RemoteFileID: b.RemoteFileID, MountRoot: b.MountRoot} // empty etag = never prunes
		if r, ok := remote[d]; ok {
			// The listing is the authority: a folder the user re-created under a
			// former share's name is NOT a root, whatever the old row said.
			row.RemoteFileID = r.FileID
			row.MountRoot = r.MountRoot
		}
		rows = append(rows, row)
		dirtied++
	}
	if len(rows) == 0 {
		return
	}
	if err := st.UpsertBaselineBatch(pk, rows); err != nil {
		slog.Warn("dir-baseline maintenance failed", "err", err)
		return
	}
	slog.Info("dir baselines maintained", "healed", healed, "dirtied", dirtied)
}

// applyPlan filters, executes, and reports a reconciliation plan — shared by the
// full SyncOnce and the scoped SyncScope, so both behave identically once a plan
// exists. actions/remote are keyed relative to the pair root regardless of scope.
// base is the baseline map the plan was diffed against; it drives the post-pass
// directory-etag maintenance (nil = remote wasn't a subtree-listing scan, only
// failure-dirtying applies).
func (e *Engine) applyPlan(ctx context.Context, st *state.Store, p Pair, actions []engine.Action, remote map[string]engine.RemoteState, base map[string]engine.BaselineState, fullScan bool) (transfer.Stats, error) {
	// The guard state could not be read. It records which folders are frozen,
	// so proceeding would resume a paused folder and apply the very changes the
	// guard stopped. Refuse every pair on this account until it is readable.
	if e.guardStateUnavailable() {
		return transfer.Stats{}, errGuardStateUnavailable()
	}
	// Filter out files the server forbids (flagged for the UI) and files the user
	// blacklisted (dropped silently).
	blset, _ := e.dirs.LoadBlacklist()
	blacklisted := func(rel string) bool {
		abs := filepath.Join(p.LocalDir, filepath.FromSlash(rel))
		return blset[config.PathKey(abs)]
	}
	actions, blocked := engine.FilterBlocked(actions, e.forbidden.Load(), e.escaper.Load(), blacklisted)
	// A full reconcile is authoritative (replace the list); a scoped/delta sync only
	// saw a few paths, so it merges its finds in without clearing the rest.
	if fullScan {
		e.setBlocked(p.LocalDir, blocked)
	} else {
		e.addBlocked(p.LocalDir, blocked)
	}
	if len(blocked) > 0 {
		slog.Debug("files blocked (server-forbidden names)", "count", len(blocked))
	}

	pk := PairKey(p.LocalDir, p.RemoteRoot)

	// A share or mount detached from the account vanishes from the listing and
	// plans as a local delete of everything under it; keep the copy instead
	// (Deck #557). Before the damage guard on purpose: a big unshare is then
	// recognised for what it is rather than frozen for review.
	kept, derr := e.keepDetached(st, pk, p, actions, base)
	if derr != nil {
		return transfer.Stats{}, derr
	}
	actions = kept

	// The damage guard: refuse a pass that looks like the SERVER lost data — a
	// restore from an old snapshot, a shared folder someone emptied, ransomware
	// encrypting in place — rather than the user changing things.
	//
	// It sits at applyPlan rather than at each entry point because SyncOnce,
	// SyncScope, SyncPaths and syncRemoteDelta all funnel through here — judging
	// per entry point would let the push/delta and watcher fast paths bypass it.
	// It complements the two guards below, which protect the SERVER from a
	// vanished local folder; this one protects the LOCAL copy, and counts files
	// being REPLACED as well as deleted, because ransomware's plan is all
	// downloads and a deletions-only check would wave every encrypted file
	// through. A trip refuses the WHOLE pass, downloads included, for the same
	// reason.
	{
		// Backstop for ensurePair's check: this is the last point before actions
		// are executed, and a freeze can be written by another pair's pass while
		// this one is scanning.
		if err := e.frozenPairErr(p); err != nil {
			return transfer.Stats{}, err
		}

		// A pass with nothing to do is judged — and spends — nothing. Otherwise
		// the first quiet 15-second poll after the user resumes a frozen folder
		// would burn the single-pass exemption they were granted, and the real
		// pass behind it would be refused all over again.
		if len(actions) > 0 {
			cloned, _ := st.CloneStatus(pk)
			exempt := e.consumeGuardExemption(p)
			if syncguard.GuardApplies(cloned, exempt) {
				known, err := st.BaselineCount(pk)
				if err != nil {
					return transfer.Stats{}, fmt.Errorf("damage guard: cannot count known files: %w", err)
				}
				// The guard needs to tell a tracked path from a new one, and
				// SyncPaths deliberately hands applyPlan a nil base (its remote
				// map is stat-built, so it must not stamp dir etags). Read the
				// rows for just this plan's paths rather than guessing: with no
				// baseline a total rewrite scores as pure additions.
				gbase, err := e.baselineForGuard(st, pk, base, actions)
				if err != nil {
					return transfer.Stats{}, err
				}
				// The listing check first: it runs on what the scan returned
				// rather than on the plan, so it catches a server folder that was
				// emptied or restored from an old snapshot before any action is
				// derived. Only a full scan's remote map is a complete listing —
				// a scoped, stat-built or delta map is a handful of paths and
				// would read as a catastrophic shrink every time.
				if fullScan {
					if reason, trips := syncguard.ScanTrips(len(remote), known, syncguard.DefaultPct); trips {
						return transfer.Stats{}, e.tripGuard(p, reason, syncguard.Counts{}, known)
					}
				}
				counts := syncguard.Count(actions, gbase)
				// Count counts actions; a folder deleted here is one action for
				// everything beneath it. Weigh it by what it holds.
				if counts.Deletions, err = deletionWeight(st, pk, actions, engine.ActDeleteLocal); err != nil {
					return transfer.Stats{}, fmt.Errorf("damage guard: cannot weigh deletions: %w", err)
				}
				if reason, trips := syncguard.Trips(counts, known, syncguard.DefaultFloor, syncguard.DefaultPct); trips {
					return transfer.Stats{}, e.tripGuard(p, reason, counts, known)
				}
			}
		}
	}

	// Files another user has open. This is derived from the same listing the plan
	// came from, so it costs nothing extra. Only paths we actually listed count: a
	// subtree pruned by its ETag is replayed from the baseline with Lock == nil,
	// which means "we did not look", not "free".
	//
	// Skipped for a backup pair: it is a server write we have no use for, since a
	// one-way folder never uploads and so never needs to know who else has a file
	// open.
	// A folder that has just left virtual-files mode may legitimately be missing
	// files locally — placeholders that never populated leave an empty directory.
	// The reconciler cannot tell that from a user deletion, so for one pass we
	// restore rather than delete. See Deck #571: this cost a real shared file.
	if e.dirs.IsPostRevert(p.LocalDir) {
		var restored []string
		missingLocally := func(rel string) bool {
			_, err := os.Lstat(filepath.Join(p.LocalDir, filepath.FromSlash(rel)))
			return err != nil
		}
		actions, restored = engine.RestoreInsteadOfDelete(actions, remote, missingLocally)
		if len(restored) > 0 {
			slog.Warn("first pass after leaving virtual files: restoring missing files instead of deleting them on the server",
				"dir", p.LocalDir, "count", len(restored))
		}
	}

	var heldUploads []string
	if e.LockingAvailable() {
		examined, lockedNow := lockScan(remote, e.Account.LoginName, p.LocalDir)
		e.reconcileLocked(p.LocalDir, examined, lockedNow)

		// Warn the local editor, but ONLY from here — the live sync path. The
		// warner writes a name carrier next to the document, and a real file
		// inside an on-demand root makes the cloud filter treat the directory as
		// already populated, so the actual placeholders never appear. Seen in the
		// field 2026-08-16: a shared folder went empty on the VM.
		if e.lockWarn != nil && e.lockoutEnabled() {
			e.lockWarn.apply(e.LockedFiles())
		}

		// Hold back uploads to files somebody else is editing. Uploading now would
		// hand THEM the conflict when they save, despite them having done nothing
		// wrong; waiting puts it on whoever ignored the warning instead.
		actions, heldUploads = engine.FilterLocked(actions, remote, e.Account.LoginName)
		for _, rel := range heldUploads {
			slog.Info("holding an upload: someone else has the file open", "path", rel)
		}
	}
	// A server copy that failed its checksum is not fetched again until it
	// changes (Deck #691, see damaged.go).
	actions, skippedDamaged := e.skipDamaged(pk, actions, remote)

	// Data-loss guard. If this plan would delete files on the SERVER while the
	// local root has vanished (folder deleted, moved, unmounted, or empty), that is
	// almost certainly a missing folder — not an intentional mass deletion. Refuse
	// the whole sync rather than propagate deletions that would wipe Nextcloud data.
	// This is the logout-then-delete-the-folder footgun that previously emptied the
	// server: a non-empty baseline + an absent local tree reads as "delete everything".
	// Weighed, not counted: one folder DELETE takes everything beneath it.
	remoteDeletes, err := deletionWeight(st, pk, actions, engine.ActDeleteRemote)
	if err != nil {
		return transfer.Stats{}, fmt.Errorf("data-loss guard: cannot weigh deletions: %w", err)
	}
	if remoteDeletes > 0 && localRootVanished(p.LocalDir) {
		slog.Error("data-loss guard: refusing to delete server files while the local folder is missing or empty",
			"local", p.LocalDir, "remote_deletes", remoteDeletes)
		e.status("Sync stopped — local folder missing")
		// Surface it: this halts syncing, so the user needs to see why (throttled,
		// since it would otherwise re-trip on every sync while the folder is gone).
		e.toastGuardTripped()
		return transfer.Stats{}, fmt.Errorf("aborting sync: local folder %q is missing or empty but %d server file(s) would be deleted — refusing to propagate deletions to protect your Nextcloud data (restore/remount the folder, or remove and re-add the sync if this was intentional)", p.LocalDir, remoteDeletes)
	}

	// Stronger guard: even when the root isn't fully empty, refuse a pass that would
	// delete a large FRACTION of the pair's known files on the server. The all-empty
	// check above misses a folder emptied in BATCHES (e.g. a move while the watcher
	// is live deletes a chunk per scoped sync) — a fraction check catches that.
	// Small or partial deletes pass through.
	if remoteDeletes >= guardDeleteFloor {
		if total, err := st.BaselineCount(PairKey(p.LocalDir, p.RemoteRoot)); err == nil && bulkDeleteGuardTrips(remoteDeletes, total) {
			slog.Error("data-loss guard: refusing a bulk server deletion",
				"local", p.LocalDir, "remote_deletes", remoteDeletes, "known_files", total)
			e.status("Sync stopped — too many deletions")
			e.toastGuardTripped()
			return transfer.Stats{}, fmt.Errorf("aborting sync: this pass would delete %d of %d file(s) on the server (%d%%) — refusing a bulk deletion to protect your Nextcloud data. If the sync folder moved or unmounted, restore it; if you really meant this, delete from the Nextcloud web UI, or remove and re-add the sync", remoteDeletes, total, remoteDeletes*100/total)
		}
	}

	if len(actions) == 0 {
		// Nothing to do — but re-listed dirs still need their etags stamped, or
		// they are re-listed on every future scan (this quiet case is the common
		// steady state: our own transfers stale the ancestor dir etags).
		maintainDirBaselines(st, pk, base, remote, append(append([]string(nil), heldUploads...), skippedDamaged...))
		if base != nil && len(skippedDamaged) == 0 {
			e.clearCheckpoint(st, pk) // clean pass — the crawl's rescue rows served their purpose
		}
		// A quiet pass must still clear "Scanning…" — nothing else will until the
		// next delta runs, which used to leave the flyout stuck for minutes.
		//
		// But a pass whose ONLY work was held is not "Up to date": the user's
		// change is deliberately unsynced and saying otherwise would have them
		// close the laptop believing it had gone.
		e.status(heldStatus(heldUploads))
		return transfer.Stats{}, nil
	}

	// Count the file transfers (and their bytes) in this plan for progress.
	transfers := 0
	var bytes int64
	for _, a := range actions {
		if a.Kind == engine.ActDownload || a.Kind == engine.ActUpload {
			transfers++
			if r, ok := remote[a.Path]; ok {
				bytes += r.Size
			}
		}
	}
	if transfers > 0 {
		e.progStart(transfers, bytes)
		defer e.progEnd()
	}

	e.status("Syncing…")
	var probMu sync.Mutex
	var problems []string // paths whose action failed — they poison dir-etag stamping
	ex := &transfer.Executor{
		Client:     e.client,
		State:      st,
		PairKey:    pk,
		LocalRoot:  p.LocalDir,
		RemoteRoot: p.RemoteRoot,
		Remote:     remote,
		Escaper:    e.escaper.Load(),
		Workers:    4,
		Policy:     e.policy,
		OnBegin: func(a engine.Action) {
			abs := filepath.Join(p.LocalDir, filepath.FromSlash(a.Path))
			e.markInflight(abs, true)
			// Our own deny-write handle would refuse our own download. Let go for
			// the duration of the transfer and re-take it afterwards.
			if e.lockWarn != nil {
				e.lockWarn.release(abs)
			}
			if a.Kind == engine.ActDownload || a.Kind == engine.ActUpload {
				e.progCurrent(filepath.Base(a.Path))
			}
		},
		OnProgress: func(a engine.Action, delta int64) {
			e.progBytes.Add(delta)
		},
		OnMovedAside: func(rel, dest string) {
			name := filepath.Base(filepath.FromSlash(rel))
			e.toast("Kept a copy of "+name, "“"+name+"” was deleted on the server and is too big for the Recycle Bin, "+
				"so it was moved to "+dest+". Delete it from there once you no longer need it.", "")
		},
		OnEvent: func(a engine.Action, aerr error) {
			abs := filepath.Join(p.LocalDir, filepath.FromSlash(a.Path))
			e.markInflight(abs, false)
			if e.lockWarn != nil {
				e.lockWarn.retake(abs) // no-op unless it is still locked by someone else
			}
			if aerr == nil && (a.Kind == engine.ActDownload || a.Kind == engine.ActUpload || a.Kind == engine.ActConflict) {
				// ActConflict included: keep-both leaves the server's fresh copy
				// at a.Path (downloaded) and uploads the conflicted twin - both
				// synced, but neither is a plain Download/Upload action, so the
				// icon stayed on the pending spinner until the next restart.
				e.statusIcons.notifySynced(p.LocalDir, a.Path)
			}
			if a.Kind == engine.ActDownload || a.Kind == engine.ActUpload {
				e.progComplete()
			}
			var damaged *transfer.ChecksumMismatchError
			if (a.Kind == engine.ActDownload || a.Kind == engine.ActConflict) && errors.As(aerr, &damaged) {
				e.noteDamaged(pk, a.Path, remote[a.Path].ETag)
			}
			if aerr != nil {
				probMu.Lock()
				problems = append(problems, a.Path)
				if a.Dest != "" {
					problems = append(problems, a.Dest)
				}
				probMu.Unlock()
			}
			ev := activity.Event{Local: p.LocalDir, Path: a.Path, Kind: a.Kind.String()}
			if a.Dest != "" {
				ev.Path = a.Path + " → " + a.Dest
			}
			ev.Err = e.recordActionResult(a, aerr) // humanised + deduped; "" on success
			e.recorder.Add(ev)
		},
	}
	stats, err := ex.Run(ctx, actions)
	// A cancelled pass (quit, pause, a restart) stops starting transfers but Run
	// still reports no error, and what it never started is in no problem list.
	// Treat it as the partway stop it is: stamping its folders as seen hid the
	// files it never fetched from every later scan (Deck #691).
	cancelled := err == nil && ctx.Err() != nil
	if cancelled {
		err = ctx.Err()
	}
	e.setConflicts(p, ex.Pending)
	// Unresolved conflicts must keep their subtrees re-scanned (a pruned dir would
	// reconstruct the conflicted file's remote state from the stale baseline and
	// the conflict would stop being re-detected).
	// A held upload is unfinished business for the same reason: if its parent
	// directory is stamped clean, the ETag prune skips the subtree and the
	// pending local change stops being re-detected until something else there
	// changes.
	problems = append(problems, heldUploads...)
	problems = append(problems, skippedDamaged...)
	for _, c := range ex.Pending {
		problems = append(problems, c.Path)
	}
	if err == nil && len(problems) == 0 && base != nil {
		// A clean full pass means the local tree now matches the server, so the
		// post-revert safety net has done its job and normal delete semantics
		// can resume.
		if e.dirs.IsPostRevert(p.LocalDir) {
			_ = e.dirs.ClearPostRevert(p.LocalDir)
			slog.Info("post-revert restore window closed", "dir", p.LocalDir)
		}
	}
	if err == nil {
		maintainDirBaselines(st, pk, base, remote, problems)
		if base != nil && len(problems) == 0 {
			e.clearCheckpoint(st, pk) // fully clean: every action landed, no conflicts pending
		}
	} else if len(problems) > 0 {
		// The pass died partway (offline, auth, cancel) — don't stamp anything,
		// but do dirty the chains of the failures that already happened.
		maintainDirBaselines(st, pk, nil, remote, problems)
	}
	switch {
	case cancelled:
		// Stopped from outside: not an error to tell anyone about, and not up
		// to date either.
	case err != nil:
		switch syncErrKind(err) {
		case "auth":
			e.status("Sign in again")
			e.authLost()
		case "offline":
			e.status("Offline")
		default:
			e.status("Error")
			e.toastError(err)
		}
	default:
		e.status("Up to date")
		e.resetAuthLost()
	}
	return stats, err
}

// SyncScope reconciles only one subtree of a pair (scope, a pair-relative dir).
// Local-change-driven syncs use it so a single edit re-scans just its branch
// instead of the whole tree. scope == "" is equivalent to SyncOnce.
func (e *Engine) SyncScope(ctx context.Context, p Pair, scope string) (transfer.Stats, error) {
	if e.Paused() {
		return transfer.Stats{}, nil
	}
	if err := e.ensurePair(ctx, p); err != nil {
		return transfer.Stats{}, err
	}
	st, err := e.getStore()
	if err != nil {
		return transfer.Stats{}, err
	}

	actions, remote, base, err := e.computePlanScoped(ctx, st, p, scope)
	if err != nil {
		return transfer.Stats{}, err
	}
	// NOTE (if this dead path is ever revived): applyPlan's clean-pass hook will
	// clear the pair's ENTIRE scan checkpoint on the evidence of this subtree-only
	// scan — scope the clear before wiring a checkpoint in here.
	return e.applyPlan(ctx, st, p, actions, remote, base, false) // scoped/delta — merge blocks, don't clear
}

// maxSyncPaths caps a targeted reconcile. A larger burst of changes falls back to
// a full sync — cheaper than hundreds of per-file PROPFINDs, and rare in practice.
const maxSyncPaths = 256

// relsFor converts watcher-reported absolute paths into clean, de-duplicated
// pair-relative paths. ok is false if the batch is empty, too large for a targeted
// reconcile, or any path lies outside the pair — callers then do a full sync.
func relsFor(localRoot string, changed []string) (rels []string, ok bool) {
	if len(changed) == 0 || len(changed) > maxSyncPaths {
		return nil, false
	}
	seen := make(map[string]struct{}, len(changed))
	for _, abs := range changed {
		rel, err := filepath.Rel(localRoot, abs)
		if err != nil {
			return nil, false
		}
		rel = filepath.ToSlash(rel)
		if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
			return nil, false
		}
		if _, dup := seen[rel]; dup {
			continue
		}
		seen[rel] = struct{}{}
		rels = append(rels, rel)
	}
	return rels, len(rels) > 0
}

// SyncPaths reconciles an explicit set of pair-relative paths without walking any
// subtree: it stats each path locally, PROPFINDs each on the server, loads just
// those baseline rows, and diffs only that set. So a local edit syncs in the time
// of a couple of round-trips no matter how large its folder is — the fix for a
// file changing directly inside a huge top-level folder costing a full-tree walk.
// Because all three maps are restricted to the same path set, the diff can only
// emit actions for those paths (no spurious deletes of unscanned siblings).
// Renames WITHIN the set are coalesced into server-side moves (see planPaths); a
// rename split across debounce batches still degrades to delete+upload and is
// left to the periodic full poll.
func (e *Engine) SyncPaths(ctx context.Context, p Pair, relPaths []string) (transfer.Stats, error) {
	if e.Paused() {
		return transfer.Stats{}, nil
	}
	if !e.beginSyncPass() {
		return transfer.Stats{}, nil
	}
	defer e.endSyncPass()
	if ex, ok := e.excludesFor(p.LocalDir, p.RemoteRoot); ok {
		p.Excludes = ex // watcher captured p at start; pick up selective-sync toggles now
	}
	if err := e.ensurePair(ctx, p); err != nil {
		return transfer.Stats{}, err
	}
	st, err := e.getStore()
	if err != nil {
		return transfer.Stats{}, err
	}
	pk := PairKey(p.LocalDir, p.RemoteRoot)

	base, err := st.LoadBaselinePaths(pk, relPaths)
	if err != nil {
		return transfer.Stats{}, err
	}
	esc := e.escaper.Load()
	ig := e.ignoreFor(p)
	local := make(map[string]engine.LocalState, len(relPaths))
	remote := make(map[string]engine.RemoteState, len(relPaths))
	trackedAbsent := false
	for _, rel := range relPaths {
		if ig.Match(rel) {
			continue // not synced, so not asked about: its Stat can't fail the pass
		}
		_, tracked := base[rel]
		if fi, serr := os.Stat(filepath.Join(p.LocalDir, filepath.FromSlash(rel))); serr == nil {
			ls := engine.LocalState{Path: rel, IsDir: fi.IsDir(), MTime: fi.ModTime()}
			if !fi.IsDir() {
				ls.Size = fi.Size()
			}
			local[rel] = ls
		}
		// Stat the server under the ESCAPED name (an escaped file is stored as
		// X<suffix>); a raw stat of X would 404 and misread it as "removed
		// remotely", deleting the local file out from under a live server copy.
		rp := strings.Trim(p.RemoteRoot+"/"+esc.Encode(rel), "/")
		ent, found, rerr := e.client.Stat(ctx, rp)
		if rerr != nil {
			// A Stat that failed is NOT a path the server lacks. Read that way
			// for a tracked path, an outage deleted a whole local folder (Deck
			// #691); and a network or 5xx failure says nothing about any path.
			// Either fails the pass. A refusal (4xx) of an untracked path keeps
			// reading as absent: at worst that plans an upload the server
			// refuses again, where failing would stall every batch it is in.
			if tracked || transport.Retryable(rerr) {
				return transfer.Stats{}, fmt.Errorf("stat %q: %w", rel, rerr)
			}
			continue
		}
		if found {
			remote[rel] = remoteStateFrom(rel, ent, base[rel].MountRoot)
		} else if tracked {
			trackedAbsent = true
		}
	}
	// A 404 is only an answer if Nextcloud gave it: a proxy in front of a
	// stopped server can 404 every path. A real server never reports the pair's
	// own folder missing, so before planning a deletion from a 404, ask it once.
	if trackedAbsent {
		root := strings.Trim(p.RemoteRoot, "/")
		if _, ok, err := e.client.Stat(ctx, root); err != nil || !ok {
			return transfer.Stats{}, fmt.Errorf("the server reported a synced item missing but cannot see the sync folder %q either (found=%v, err=%v); not deleting anything on that answer", "/"+root, ok, err)
		}
	}

	actions := planPaths(base, remote, local, func(rel string) (string, error) {
		return transfer.SHA1File(filepath.Join(p.LocalDir, filepath.FromSlash(rel)))
	})
	pruneDeadBaselines(st, pk, base, remote, local)
	// nil base: this remote map is stat-built (not a subtree listing), so it must
	// never stamp dir etags — only failure-dirtying applies inside applyPlan.
	return e.applyPlan(ctx, st, p, actions, remote, nil, false) // scoped/delta — merge blocks, don't clear
}

// remoteStateFrom converts a single Stat result into a RemoteState. It exists so
// the hand-built map in SyncPaths cannot drift from what a real listing produces
// — lock state was nearly missed exactly that way, which would have left the
// feature working on cold scans and dead on the path an Office save takes.
//
// Note this is a Stat, not a listing: fields a depth-0 PROPFIND does not carry
// meaningfully here (SHA1, ReadOnly, LastModified) are deliberately left unset,
// as they were before.
//
// A share's root is told apart from anything inside it only by its PARENT's
// permissions, which a Stat doesn't see. So the flag is kept from wasRoot, the
// baseline row, while the Stat still shows it on a share or mount: leaving it
// unset wrote rows that forgot a share root, and an unshare then recycled the
// copy instead of keeping it (#557, Deck #691).
func remoteStateFrom(rel string, ent transport.Entry, wasRoot bool) engine.RemoteState {
	return engine.RemoteState{
		Path:      rel,
		IsDir:     ent.IsDir,
		ETag:      ent.ETag,
		FileID:    ent.FileID,
		Size:      ent.Size,
		Lock:      ent.Lock,
		LockKnown: true, // a Stat DID look, so its answer is authoritative
		MountRoot: wasRoot && ent.OnMount(),
	}
}

// planPaths diffs an explicit path set and then coalesces renames within it.
//
// A move arrives at the watcher as two paths — old gone, new appeared — and
// diffed independently that is a server-side delete plus a full re-upload of
// content the server already has. It also read as "Deleted on server" in the
// activity feed, which is accurate but alarming. Both halves are in the same
// batch, so the pair can be matched (new file's SHA1+size against the vanishing
// baseline row) and rewritten into a single server-side MOVE.
//
// Only moves whose old and new paths land in the SAME batch are caught. A move
// split across debounce windows still degrades to delete+upload, because the
// delete has already been applied by the time the new path shows up — the
// periodic full pass is what covers that case.
func planPaths(
	base map[string]engine.BaselineState,
	remote map[string]engine.RemoteState,
	local map[string]engine.LocalState,
	hashLocal func(rel string) (string, error),
) []engine.Action {
	return engine.CoalesceRenames(engine.Diff(base, remote, local), base, remote, local, hashLocal)
}

// syncRemoteDelta is the fast path for a notify_push. The push only says "the
// server changed" — not what — so instead of a full SyncOnce (whose cost is
// dominated by walking all local files), it runs only the ETag-pruned remote
// scan, finds the paths whose server state differs from the baseline, and
// reconciles just those with a proper three-way diff (so a concurrent local edit
// still surfaces as a conflict rather than being clobbered). Local changes the
// watcher missed are caught by the startup and periodic-poll full syncs.
func (e *Engine) syncRemoteDelta(ctx context.Context, p Pair) (transfer.Stats, error) {
	if e.Paused() {
		return transfer.Stats{}, nil
	}
	// Mirror SyncOnce: a move in progress means skip (the delta call below may
	// also delegate to SyncOnce, whose own guard is a no-op while we hold this).
	if !e.beginSyncPass() {
		return transfer.Stats{}, nil
	}
	defer e.endSyncPass()
	if ex, ok := e.excludesFor(p.LocalDir, p.RemoteRoot); ok {
		p.Excludes = ex // watcher captured p at start; pick up selective-sync toggles now
	}
	if err := e.ensurePair(ctx, p); err != nil {
		return transfer.Stats{}, err
	}
	defer e.releaseHeap() // the remote scan builds a full transient map — hand it back
	st, err := e.getStore()
	if err != nil {
		return transfer.Stats{}, err
	}
	pk := PairKey(p.LocalDir, p.RemoteRoot)
	// Before the initial clone finishes, a push should drive the clone, not a
	// delta against an incomplete baseline.
	if status, _ := st.CloneStatus(pk); status != "done" {
		return e.SyncOnce(ctx, p)
	}

	tBase := time.Now()
	base, err := st.LoadBaseline(pk)
	if err != nil {
		return transfer.Stats{}, err
	}
	baselineLoad := time.Since(tBase)
	// Build the ignore matcher up front so it also prunes the PROPFIND descent
	// (not just filters the result) — ignored trees never get walked on the server.
	ig := e.ignoreFor(p)
	tScan := time.Now()
	cp := newScanCheckpoint(st, pk)
	remote, err := engine.RemoteScan(ctx, e.client, p.RemoteRoot, engine.ScanOpts{
		Base: base, Skip: ig.Match, OnEncrypted: e.noteEncrypted, Esc: e.escaper.Load(),
		Checkpoint: cp,
	})
	cp.logSummary()
	if _, _, saves := cp.stats(); saves > 0 {
		e.markCheckpointDirty(pk)
	}
	if err != nil {
		return transfer.Stats{}, fmt.Errorf("remote scan: %w", err)
	}
	remoteScan := time.Since(tScan)
	tDelta := time.Now()

	// Restrict to paths whose server state diverged from the baseline: new,
	// content-changed (etag), type-changed, or removed on the server. Everything
	// else RemoteScan filled from the baseline, so it compares equal here.
	baseSub := make(map[string]engine.BaselineState)
	remoteSub := make(map[string]engine.RemoteState)
	for path, r := range remote {
		if b, ok := base[path]; !ok || b.RemoteETag != r.ETag || b.IsDir != r.IsDir {
			remoteSub[path] = r
			if ok {
				baseSub[path] = b
			}
		}
	}
	for path, b := range base {
		if _, ok := remote[path]; !ok {
			baseSub[path] = b // gone from the server
		}
	}
	if len(baseSub) == 0 && len(remoteSub) == 0 {
		slog.Info("remote-delta timing (no changes)",
			"baseline_load", baselineLoad.Round(time.Millisecond),
			"remote_scan", remoteScan.Round(time.Millisecond),
			"baseline_rows", len(base))
		e.clearCheckpoint(st, pk) // a quiet delta is a clean pass
		e.status("Up to date")
		return transfer.Stats{}, nil
	}

	// Local state for just the affected paths — cheap stats, no tree walk.
	local := make(map[string]engine.LocalState)
	addLocal := func(rel string) {
		if _, dup := local[rel]; dup {
			return
		}
		if fi, serr := os.Stat(filepath.Join(p.LocalDir, filepath.FromSlash(rel))); serr == nil {
			ls := engine.LocalState{Path: rel, IsDir: fi.IsDir(), MTime: fi.ModTime()}
			if !fi.IsDir() {
				ls.Size = fi.Size()
			}
			local[rel] = ls
		}
	}
	for rel := range remoteSub {
		addLocal(rel)
	}
	for rel := range baseSub {
		addLocal(rel)
	}

	ig.FilterLocal(local)
	ig.FilterRemote(remoteSub)

	actions := engine.Diff(baseSub, remoteSub, local)
	pruneDeadBaselines(st, pk, baseSub, remoteSub, local)
	deltaCompute := time.Since(tDelta)
	slog.Info("remote-delta timing",
		"baseline_load", baselineLoad.Round(time.Millisecond),
		"remote_scan", remoteScan.Round(time.Millisecond),
		"delta_compute", deltaCompute.Round(time.Millisecond),
		"baseline_rows", len(base), "changed_paths", len(remoteSub)+len(baseSub), "actions", len(actions))
	return e.applyPlan(ctx, st, p, actions, remoteSub, base, false) // remote-delta — merge blocks, don't clear
}

// maxScopes caps how many distinct subtrees one change-batch will sync before
// falling back to a full pass (changes too scattered to be worth scoping).
const maxScopes = 24

// scopesFor maps a batch of changed absolute paths to the minimal set of
// pair-relative parent directories covering them. ok is false — meaning the
// caller should do a full sync — when a change is at the pair root, falls outside
// the pair, or the batch spans more than maxScopes branches.
func scopesFor(localRoot string, changed []string) (scopes []string, ok bool) {
	set := make(map[string]struct{})
	for _, abs := range changed {
		rel, err := filepath.Rel(localRoot, abs)
		if err != nil {
			return nil, false
		}
		rel = filepath.ToSlash(rel)
		if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
			return nil, false // outside the pair
		}
		dir := parentDirOf(rel) // sync the changed path's parent subtree
		if dir == "" {
			return nil, false // root-level change — full sync
		}
		set[dir] = struct{}{}
		if len(set) > maxScopes {
			return nil, false
		}
	}
	if len(set) == 0 {
		return nil, false
	}
	// Drop any directory already covered by an ancestor in the set.
	for d := range set {
		for a := parentDirOf(d); a != ""; a = parentDirOf(a) {
			if _, covered := set[a]; covered {
				delete(set, d)
				break
			}
		}
	}
	for d := range set {
		scopes = append(scopes, d)
	}
	return scopes, true
}

// parentDirOf returns the parent directory of a "/"-separated relative path, or
// "" for a top-level entry.
func parentDirOf(rel string) string {
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		return rel[:i]
	}
	return ""
}

// toastError shows a sync-error toast, throttled to at most once every 5 minutes
// so a flaky connection doesn't spam the desktop.
func (e *Engine) toastError(err error) {
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
	msg := err.Error()
	if len(msg) > 120 {
		msg = msg[:117] + "…"
	}
	e.toast("Sync problem", msg, "")
}

// toastGuardTripped tells the user the data-loss guard paused syncing because the
// local folder vanished — throttled (shared with toastError) so it shows once, not
// on every retry while the folder is missing.
func (e *Engine) toastGuardTripped() {
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
	e.toast("Nimbo — syncing paused",
		"Your sync folder is missing or empty, so syncing is paused to protect your Nextcloud files. Restore or remount the folder to resume.", "")
}

// Run starts continuous syncing of the given pairs and blocks until ctx is
// cancelled. Watched folders can be added/removed afterwards with ReloadPairs.
// onSync, if non-nil, is called after each pair sync with its stats.
// devIgnores are dependency/VCS trees seeded into the user-editable global
// ignore list (Settings → Exclusions) rather than hard-coded in the engine:
// excluded by default — enumerating them (one PROPFIND per directory, tens of
// thousands under a typical code backup) can drive a small Nextcloud host into
// the ground — but visible, and removable by users who accept that cost.
var devIgnores = []string{"node_modules", ".git", ".svn", ".hg"}

// seedDevIgnores is a one-time migration adding devIgnores to the global ignore
// list. The DevIgnoresSeeded flag makes it once-ever, so a user who deletes a
// pattern to sync those trees doesn't have it silently re-imposed. Must run
// before the first sync pass so the patterns prune the very next remote scan.
func (e *Engine) seedDevIgnores() {
	s, err := e.dirs.LoadSettings()
	if err != nil || s.DevIgnoresSeeded {
		return
	}
	pats, _ := e.dirs.LoadIgnore()
	have := make(map[string]bool, len(pats))
	for _, p := range pats {
		have[p] = true
	}
	added := false
	for _, p := range devIgnores {
		if !have[p] {
			pats = append(pats, p)
			added = true
		}
	}
	if added {
		if err := e.dirs.SaveIgnore(pats); err != nil {
			slog.Warn("could not seed default dev ignores", "err", err)
			return // retry next start rather than marking done
		}
		slog.Info("seeded dev-tree ignore patterns", "patterns", devIgnores)
	}
	// Re-read rather than saving back the settings loaded above: this runs at
	// engine start, concurrently with whatever the caller is doing, and the file
	// I/O in between is a wide window in which another change can land. Writing
	// back the stale copy is how a SetBaseDir issued right after Start could be
	// silently reverted.
	if err := e.dirs.UpdateSettings(func(s *config.Settings) { s.DevIgnoresSeeded = true }); err != nil {
		slog.Warn("could not persist dev-ignore migration flag", "err", err)
	}
}

// pollIntervalFor picks the remote poll cadence. Without push the poll is the
// only source of server→local changes, but 15s meant ~240 PROPFINDs/hour
// against servers that never asked for it (#599) — 30s matches the official
// client. With push connected the poll is only a safety net for missed events;
// 5 minutes keeps the worst case for a dropped push event tolerable (a 15m
// backoff was tried for #599 and felt too long) at 12 PROPFINDs/hour.
func pollIntervalFor(pushAvailable bool) time.Duration {
	if pushAvailable {
		return 5 * time.Minute
	}
	return 30 * time.Second
}

func (e *Engine) Run(ctx context.Context, pairs []Pair, onSync func(Pair, transfer.Stats)) error {
	e.onSync = onSync
	_ = e.reloadGuardState()    // before any watcher fires its first scan
	e.seedDevIgnores()          // before any watcher fires its first scan
	go e.sharesRefreshLoop(ctx) // keeps the shared-folder markers current
	go e.presenceLoop(ctx)      // keeps the user's Nextcloud presence "online"
	go e.routeLoop(ctx)         // re-tries the local network address while on public
	defer e.closeStoreFinal()   // resident baseline cache lives only while running
	// Drop backup entries whose folder is gone. Here, and only here: the config
	// is quiescent, no watcher exists yet, and a stale entry is otherwise both
	// permanent and invisible (BackupViews iterates over pairs).
	e.sweepGuardState()
	e.watchMu.Lock()
	// runCtx is published under watchMu — startWatcher derives watcher
	// contexts from it under the same lock, so it can never observe a torn
	// or nil value once set.
	e.runCtx = ctx
	e.watchers = make(map[string]context.CancelFunc)
	e.triggers = make(map[string]chan struct{})
	e.triggersFull = make(map[string]chan struct{})
	e.watchDone = make(map[string]chan struct{})
	e.watchMu.Unlock()

	_ = e.notifier.Prime(ctx)

	e.pollInterval = pollIntervalFor(e.PushAvailable())
	if e.PushAvailable() {
		go e.runPush(ctx)
	}
	// State this once at startup: "nothing ever shows up under In use" has two
	// very different causes, and without this line they look identical in a log.
	if e.LockingAvailable() {
		slog.Info("file locking available (files_lock)")
	} else {
		slog.Info("file locking unavailable — the server has no files_lock app; who-has-a-file-open will stay empty")
	}
	e.status("Up to date")

	for _, p := range pairs {
		e.startWatcher(p)
	}
	go e.watchPause(ctx)      // resume/pause at timed expiry and schedule boundaries
	go e.runLockLifetime(ctx) // sweep, heartbeat and release the locks WE hold
	go e.logRequestVolume(ctx)
	// Attic retention runs on its own clock, deliberately NOT off the sync loop:
	// every sync entry point early-returns while paused and quiet hours
	// auto-pauses daily, so a purge driven from there would strand indefinitely
	// and the attic would grow without bound.
	<-ctx.Done()
	// Wait for in-flight sync passes before the deferred closeStoreFinal
	// closes the state DB under them; a Stop-then-Start cycle also can't
	// overlap two engines' watchers on the same folders and DB this way.
	e.drainWatchers(30 * time.Second)
	return nil
}

// logRequestVolume writes one line every 15 minutes saying how many requests
// went to the server since the last line, per method. Deck #599 (17.5M login
// rows from one client) could not be traced from the log because it records
// sync passes and transfers, never the requests behind them; this line is the
// answer to "what is Nimbo actually sending", in every log, without verbose.
// Quiet intervals are skipped so an idle client doesn't fill its log.
func (e *Engine) logRequestVolume(ctx context.Context) {
	const every = 15 * time.Minute
	t := time.NewTicker(every)
	defer t.Stop()
	last := e.client.RequestCounts()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cur := e.client.RequestCounts()
			attrs := []any{"per", every.String()}
			var total int64
			methods := make([]string, 0, len(cur))
			for m := range cur {
				methods = append(methods, m)
			}
			sort.Strings(methods) // stable order so lines diff cleanly
			for _, m := range methods {
				if n := cur[m] - last[m]; n > 0 {
					attrs = append(attrs, m, n)
					total += n
				}
			}
			last = cur
			if total > 0 {
				slog.Info("http requests", append(attrs, "total", total)...)
			}
		}
	}
}

// watchPause re-evaluates the effective pause state periodically so a timed
// pause expires and scheduled quiet-hours start/end without a manual toggle.
func (e *Engine) watchPause(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	last := e.Paused()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if cur := e.Paused(); cur != last {
				last = cur
				e.pauseChanged()
			}
		}
	}
}

// ReloadPairs reconciles the watched folders with the configured sync pairs:
// it starts watchers for newly-added pairs and stops them for removed ones.
// Safe to call while Run is active.
func (e *Engine) ReloadPairs() error {
	pairs, err := e.dirs.LoadPairs()
	if err != nil {
		return err
	}
	desired := make(map[string]Pair, len(pairs))
	for _, p := range pairs {
		desired[PairKey(p.LocalDir, p.RemoteRoot)] = Pair{LocalDir: p.LocalDir, RemoteRoot: p.RemoteRoot, Excludes: p.Excludes}
	}

	e.watchMu.Lock()
	var toStop []string
	for key := range e.watchers {
		if _, ok := desired[key]; !ok {
			toStop = append(toStop, key)
		}
	}
	e.watchMu.Unlock()

	for _, key := range toStop {
		e.stopWatcher(key)
	}
	for _, p := range desired {
		e.startWatcher(p)
	}
	return nil
}

// startWatcher begins watching a pair (no-op if already watched, or if Run
// hasn't started: a watcher must derive from the run context or it could
// never be stopped. A pair recorded before Run — e.g. mountSecondaryOnDemand's
// AddSyncPair, or a mobile AddSyncFolder racing Start — stays in config, and
// watching begins when Run (or the next ReloadPairs while running) starts it).
func (e *Engine) startWatcher(p Pair) {
	key := PairKey(p.LocalDir, p.RemoteRoot)
	e.watchMu.Lock()
	if e.runCtx == nil {
		e.watchMu.Unlock()
		return
	}
	if _, ok := e.watchers[key]; ok {
		e.watchMu.Unlock()
		return
	}
	cctx, cancel := context.WithCancel(e.runCtx)
	ext := make(chan struct{}, 1)
	fullExt := make(chan struct{}, 1)
	done := make(chan struct{})
	e.watchers[key] = cancel
	e.triggers[key] = ext
	e.triggersFull[key] = fullExt
	e.watchDone[key] = done
	e.watchMu.Unlock()

	go func() {
		defer close(done) // let stopWatcherSync wait for an in-flight sync to drain
		_ = e.ensurePair(cctx, p)
		syncFn := func(ctx context.Context, changed []string) error {
			// An editor's lock file appearing or vanishing is how we learn a document
			// was opened or closed. This is the last point those paths exist: relsFor
			// and then the ignore filter drop them a few frames from here.
			e.handleEditorLockFiles(ctx, p, changed)

			// Local edits identify their exact paths, so reconcile just those files
			// (no subtree walk — instant even inside a huge folder). A full pass
			// (nil changed — startup, poll, push) reconciles the whole pair.
			var stats transfer.Stats
			var err error
			if rels, ok := relsFor(p.LocalDir, changed); ok {
				stats, err = e.SyncPaths(ctx, p, rels)
			} else {
				stats, err = e.SyncOnce(ctx, p)
			}
			// Record per-folder health before returning: the engine's status
			// string is global, so this is the only place a UI can learn that
			// THIS folder stopped syncing while others are fine.
			e.notePairResult(p, err)
			if err != nil {
				slog.Error("sync failed", "local", p.LocalDir, "err", err)
				return err
			}
			if e.onSync != nil {
				e.onSync(p, stats)
			}
			if !e.PushAvailable() {
				_, _ = e.notifier.Check(ctx)
			}
			return nil
		}
		// A push only signals "the server changed" (no path), so reconcile the
		// remote delta instead of a full local-walking sync.
		pushFn := func(ctx context.Context) error {
			stats, err := e.syncRemoteDelta(ctx, p)
			e.notePairResult(p, err)
			if err == nil && e.onSync != nil {
				e.onSync(p, stats)
			}
			return err
		}
		_ = watch.Run(cctx, watch.Options{
			Root:          p.LocalDir,
			PollInterval:  e.pollInterval,
			Debounce:      500 * time.Millisecond, // snappy local→server; still coalesces a burst
			External:      ext,
			FullSync:      fullExt,
			OnPush:        pushFn,
			FullSyncEvery: time.Hour, // most polls are fast remote-deltas; full walk hourly
		}, syncFn)

		e.watchMu.Lock()
		delete(e.watchers, key)
		delete(e.triggers, key)
		delete(e.triggersFull, key)
		delete(e.watchDone, key)
		e.watchMu.Unlock()
	}()
}

// stopWatcher cancels a pair's watcher and clears its deferred conflicts/blocks.
func (e *Engine) stopWatcher(key string) {
	e.watchMu.Lock()
	cancel := e.watchers[key]
	delete(e.watchers, key)
	delete(e.triggers, key)
	e.watchMu.Unlock()
	e.forgetPairHealth(key) // a folder that is gone is not a folder that is broken
	if cancel != nil {
		cancel()
	}
}

// stopWatcherSync cancels a pair's watcher AND waits for its goroutine — hence
// any in-flight sync — to fully exit, so the caller can mutate the folder (e.g.
// move it) with no sync racing it and misreading the change. The watch loop runs
// syncs serially and returns only after the current one finishes, so by the time
// the goroutine exits no sync is touching the folder. Bounded so a wedged sync
// can't hang the caller forever.
func (e *Engine) stopWatcherSync(key string) {
	e.watchMu.Lock()
	cancel := e.watchers[key]
	done := e.watchDone[key]
	delete(e.watchers, key)
	delete(e.triggers, key)
	e.watchMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			slog.Warn("watcher did not stop within 30s; proceeding", "key", key)
		}
	}
}

// runPush connects to notify_push and fans events out to all active watchers.
func (e *Engine) runPush(ctx context.Context) {
	c := push.New(e.caps.NotifyPush.Websocket, e.Account.LoginName, e.secret)
	c.SetHTTPClient(e.client.HTTPClient()) // follow the local network route; share the session
	c.SetStatusFunc(e.setPushState)
	_ = c.Run(ctx, func(ev push.Event) {
		switch ev.Type {
		case "notify_file":
			e.TriggerSync()
			if e.onFilesChanged != nil {
				e.onFilesChanged()
			}
		case "notify_notification":
			go func() {
				if _, err := e.notifier.Check(ctx); err != nil {
					slog.Warn("notification check failed", "err", err)
				}
				// A new incoming share announces itself as a notification, so
				// this is the moment share markers are stale (throttled inside).
				e.refreshSharesSoon(ctx)
			}()
		}
	})
}
