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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/otherworld/nimbo/internal/account"
	"github.com/otherworld/nimbo/internal/activity"
	"github.com/otherworld/nimbo/internal/config"
	"github.com/otherworld/nimbo/internal/engine"
	"github.com/otherworld/nimbo/internal/notify"
	"github.com/otherworld/nimbo/internal/policy"
	"github.com/otherworld/nimbo/internal/push"
	"github.com/otherworld/nimbo/internal/state"
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

	// failLog dedupes per-action sync failures so a permanently-rejected item
	// (e.g. something dropped into the app-managed .Collectives folder) is logged
	// once with a human-readable reason instead of spamming every sync pass.
	failMu   sync.Mutex
	lastFail map[string]string // "<kind>\x00<path>" -> last human reason logged

	policy       transfer.ConflictPolicy
	conflictMu   sync.Mutex
	conflicts    map[string][]ConflictItem // key = pair LocalDir
	conflictSubs []chan struct{}

	inflightMu       sync.Mutex
	inflight         map[string]bool // absolute paths currently being transferred
	onOverlayRefresh func(string)    // notified when a path's sync state changes

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
	}
	eng.forbidden.Store(forbidden)
	eng.escaper.Store(escaper)
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

// DownloadRange returns up to length bytes of a remote file starting at offset
// (used to hydrate on-demand placeholders).
func (e *Engine) DownloadRange(ctx context.Context, remotePath string, offset, length int64) ([]byte, error) {
	body, _, _, err := e.client.GetFrom(ctx, remotePath, offset)
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

// Upload pushes localPath's content to the files-root-relative remotePath,
// creating parent collections as needed (chunked for large files). Used by the
// on-demand write-back watcher; it does NOT touch the diff-engine baseline.
func (e *Engine) Upload(ctx context.Context, localPath, remotePath string) error {
	remotePath = strings.Trim(remotePath, "/")
	if i := strings.LastIndex(remotePath, "/"); i > 0 {
		if err := e.client.EnsureCollection(ctx, remotePath[:i]); err != nil {
			return err
		}
	}
	_, err := transfer.Upload(ctx, e.client, localPath, remotePath)
	return err
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
	s, _ := e.dirs.LoadSettings()
	for _, p := range s.PinnedApps {
		if p == id {
			return nil
		}
	}
	s.PinnedApps = append(s.PinnedApps, id)
	return e.dirs.SaveSettings(s)
}

// UnpinApp removes an app from the pinned list.
func (e *Engine) UnpinApp(id string) error {
	s, _ := e.dirs.LoadSettings()
	out := s.PinnedApps[:0]
	for _, p := range s.PinnedApps {
		if p != id {
			out = append(out, p)
		}
	}
	s.PinnedApps = out
	return e.dirs.SaveSettings(s)
}

// Favorites returns the user's favourited files and folders.
func (e *Engine) Favorites(ctx context.Context) ([]transport.Entry, error) {
	return e.client.Favorites(ctx)
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
	s, _ := e.dirs.LoadSettings()
	s.UploadKBps, s.DownloadKBps = up, down
	if err := e.dirs.SaveSettings(s); err != nil {
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
	s, _ := e.dirs.LoadSettings()
	s.BaseDir = dir
	return e.dirs.SaveSettings(s)
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
	pairs = append(pairs, config.SyncPair{LocalDir: localDir, RemoteRoot: remoteRoot})
	if err := e.dirs.SavePairs(pairs); err != nil {
		return err
	}
	return e.ReloadPairs()
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
		// Drop the pair's checkpoint rows — cached dir listings are the big
		// blobs, and nothing else ever targets this pair_key again. Best-effort;
		// the 14-day age-out is the backstop.
		if st, serr := e.getStore(); serr == nil {
			if cerr := st.ClearScanCheckpoint(PairKey(removed.LocalDir, removed.RemoteRoot)); cerr != nil {
				slog.Warn("scan checkpoint clear on remove failed", "err", cerr)
			}
		}
	}
	if deleteLocal && removed != nil {
		return os.RemoveAll(removed.LocalDir)
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
	pairs, _ := e.dirs.LoadPairs()
	inPair := false
	for _, p := range pairs {
		rel, err := filepath.Rel(filepath.Clean(p.LocalDir), abs)
		if err != nil {
			continue
		}
		rel = filepath.ToSlash(rel)
		if rel == ".." || strings.HasPrefix(rel, "../") {
			continue
		}
		inPair = true
		break
	}
	if !inPair {
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
	return "ok"
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
	if err := os.MkdirAll(p.LocalDir, 0o755); err != nil {
		return err
	}
	if p.RemoteRoot != "" {
		if err := e.client.EnsureCollection(ctx, p.RemoteRoot); err != nil {
			return fmt.Errorf("ensure remote root: %w", err)
		}
	}
	return nil
}

// computePlan scans both sides, diffs against the baseline, and coalesces renames.
func (e *Engine) computePlan(ctx context.Context, st *state.Store, p Pair) ([]engine.Action, map[string]engine.RemoteState, map[string]engine.BaselineState, error) {
	pk := PairKey(p.LocalDir, p.RemoteRoot)
	base, err := st.LoadBaseline(pk)
	if err != nil {
		return nil, nil, nil, err
	}
	// Exclude ignored paths from both sides so they're left untouched (not synced,
	// not deleted): global patterns + this pair's own excludes. Passing ig.Match to
	// RemoteScan also prunes the PROPFIND descent so ignored trees (node_modules,
	// .git, …) don't hammer the server; FilterRemote then stays as a safety net.
	globalIgnore, _ := e.dirs.LoadIgnore()
	ig := engine.NewIgnore(append(append([]string{}, globalIgnore...), p.Excludes...))
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
		return nil, nil, nil, fmt.Errorf("remote scan: %w", err)
	}
	local, err := engine.LocalScan(p.LocalDir)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("local scan: %w", err)
	}
	ig.FilterLocal(local)
	ig.FilterRemote(remote)

	actions := engine.Diff(base, remote, local)
	pruneDeadBaselines(st, pk, base, remote, local)
	hashLocal := func(rel string) (string, error) {
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
	globalIgnore, _ := e.dirs.LoadIgnore()
	ig := engine.NewIgnore(append(append([]string{}, globalIgnore...), p.Excludes...))
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
	switch status, _ := st.CloneStatus(pk); {
	case status == "done":
		// Proceed to the diff path below. Backfill the config-side sync-history
		// marker: pairs that finished their clone before the marker existed must
		// still be covered by the state-reset tripwire.
		e.markPairSyncedOnce(pk)
	case status == "started":
		e.status("Syncing…")
		return e.cloneRemote(ctx, st, p)
	default:
		if empty, _ := st.BaselineEmpty(pk); empty {
			e.warnIfStateReset(pk, p) // synced before but no state? say so before the takeover
			e.status("Syncing…")
			return e.cloneRemote(ctx, st, p)
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
	return e.applyPlan(ctx, st, p, actions, remote, base, true) // full reconcile
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
	globalIgnore, _ := e.dirs.LoadIgnore()
	ig := engine.NewIgnore(append(append([]string{}, globalIgnore...), p.Excludes...))

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
			switch decideCloneFile(takeover, fi, isDehydratedPlaceholder(fi), r) {
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
	for _, en := range rootEntries {
		full := strings.Trim(en.Path, "/")
		if full == root {
			continue
		}
		rel := relOf(full)
		rootRemote[rel] = engine.RemoteState{Path: rel, IsDir: en.IsDir, ETag: en.ETag, FileID: en.FileID, Size: en.Size, LastModified: en.LastModified}
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
			for _, en := range entries {
				rel := relOf(strings.Trim(en.Path, "/"))
				if rel == "" {
					continue
				}
				sub[rel] = engine.RemoteState{Path: rel, IsDir: en.IsDir, ETag: en.ETag, FileID: en.FileID, Size: en.Size, LastModified: en.LastModified}
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
// placeholder — see isDehydratedPlaceholder), which is always fetched instead.
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
	dt := localFI.ModTime().Unix() - r.LastModified.Unix()
	if localFI.Size() == r.Size && !r.LastModified.IsZero() && dt >= -2 && dt <= 2 {
		return cloneAdopt
	}
	return cloneSkip
}

// baselineForLocal builds a baseline row for a file already present locally
// (size/mtime from disk, etag/fileid from the server) so a resumed clone keeps it
// out of future re-downloads. ContentSHA1 is left empty; a later sync fills it.
func baselineForLocal(localRoot, rel string, r engine.RemoteState) engine.BaselineState {
	b := engine.BaselineState{Path: rel, RemoteETag: r.ETag, RemoteFileID: r.FileID, LocalSize: r.Size}
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
// subtree fully reconciled, and dirty (empty etag) the ancestor chains of every
// path that failed or conflicted — a stamped ancestor must never hide unfinished
// work (e.g. a freshly created dir whose child download failed would otherwise
// be pruned over and the child never retried). Stamping the SCAN-time etag is
// TOCTOU-safe: anything the server changed after the scan carries a newer etag,
// so it still fails the prune next pass.
//
// remote must come from a real subtree-listing scan (full/scoped/delta): its dir
// entries carry scan-time etags and imply the subtree was walked. Pass base=nil
// when it doesn't (SyncPaths' stat-built map) — then only dirtying runs.
func maintainDirBaselines(st *state.Store, pk string, base map[string]engine.BaselineState, remote map[string]engine.RemoteState, problems []string) {
	dirty := make(map[string]struct{})
	for _, pth := range problems {
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
			if b, ok := base[pth]; ok && b.IsDir && b.RemoteETag == r.ETag {
				continue // still fresh — was pruned or genuinely unchanged
			}
			rows = append(rows, engine.BaselineState{Path: pth, IsDir: true, RemoteETag: r.ETag, RemoteFileID: r.FileID})
		}
	}
	healed := len(rows)
	for d := range dirty {
		row := engine.BaselineState{Path: d, IsDir: true} // empty etag = never prunes
		if r, ok := remote[d]; ok {
			row.RemoteFileID = r.FileID
		} else if b, ok := base[d]; ok {
			row.RemoteFileID = b.RemoteFileID
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return
	}
	if err := st.UpsertBaselineBatch(pk, rows); err != nil {
		slog.Warn("dir-baseline maintenance failed", "err", err)
		return
	}
	slog.Info("dir baselines maintained", "healed", healed, "dirtied", len(dirty))
}

// applyPlan filters, executes, and reports a reconciliation plan — shared by the
// full SyncOnce and the scoped SyncScope, so both behave identically once a plan
// exists. actions/remote are keyed relative to the pair root regardless of scope.
// base is the baseline map the plan was diffed against; it drives the post-pass
// directory-etag maintenance (nil = remote wasn't a subtree-listing scan, only
// failure-dirtying applies).
func (e *Engine) applyPlan(ctx context.Context, st *state.Store, p Pair, actions []engine.Action, remote map[string]engine.RemoteState, base map[string]engine.BaselineState, fullScan bool) (transfer.Stats, error) {
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

	// Data-loss guard. If this plan would delete files on the SERVER while the
	// local root has vanished (folder deleted, moved, unmounted, or empty), that is
	// almost certainly a missing folder — not an intentional mass deletion. Refuse
	// the whole sync rather than propagate deletions that would wipe Nextcloud data.
	// This is the logout-then-delete-the-folder footgun that previously emptied the
	// server: a non-empty baseline + an absent local tree reads as "delete everything".
	remoteDeletes := 0
	for _, a := range actions {
		if a.Kind == engine.ActDeleteRemote {
			remoteDeletes++
		}
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

	pk := PairKey(p.LocalDir, p.RemoteRoot)
	if len(actions) == 0 {
		// Nothing to do — but re-listed dirs still need their etags stamped, or
		// they are re-listed on every future scan (this quiet case is the common
		// steady state: our own transfers stale the ancestor dir etags).
		maintainDirBaselines(st, pk, base, remote, nil)
		if base != nil {
			e.clearCheckpoint(st, pk) // clean pass — the crawl's rescue rows served their purpose
		}
		// A quiet pass must still clear "Scanning…" — nothing else will until the
		// next delta runs, which used to leave the flyout stuck for minutes.
		e.status("Up to date")
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
			e.markInflight(filepath.Join(p.LocalDir, filepath.FromSlash(a.Path)), true)
			if a.Kind == engine.ActDownload || a.Kind == engine.ActUpload {
				e.progCurrent(filepath.Base(a.Path))
			}
		},
		OnProgress: func(a engine.Action, delta int64) {
			e.progBytes.Add(delta)
		},
		OnEvent: func(a engine.Action, aerr error) {
			e.markInflight(filepath.Join(p.LocalDir, filepath.FromSlash(a.Path)), false)
			if a.Kind == engine.ActDownload || a.Kind == engine.ActUpload {
				e.progComplete()
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
	e.setConflicts(p, ex.Pending)
	// Unresolved conflicts must keep their subtrees re-scanned (a pruned dir would
	// reconstruct the conflicted file's remote state from the stale baseline and
	// the conflict would stop being re-detected).
	for _, c := range ex.Pending {
		problems = append(problems, c.Path)
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
	if err != nil {
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
	} else {
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
// emit actions for those paths (no spurious deletes of unscanned siblings). Renames
// spanning the set aren't coalesced here; the periodic full poll handles those.
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
	local := make(map[string]engine.LocalState, len(relPaths))
	remote := make(map[string]engine.RemoteState, len(relPaths))
	for _, rel := range relPaths {
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
		if ent, found, rerr := e.client.Stat(ctx, rp); rerr == nil && found {
			remote[rel] = engine.RemoteState{Path: rel, IsDir: ent.IsDir, ETag: ent.ETag, FileID: ent.FileID, Size: ent.Size}
		}
	}

	globalIgnore, _ := e.dirs.LoadIgnore()
	ig := engine.NewIgnore(append(append([]string{}, globalIgnore...), p.Excludes...))
	ig.FilterLocal(local)
	ig.FilterRemote(remote)

	actions := engine.Diff(base, remote, local)
	pruneDeadBaselines(st, pk, base, remote, local)
	// nil base: this remote map is stat-built (not a subtree listing), so it must
	// never stamp dir etags — only failure-dirtying applies inside applyPlan.
	return e.applyPlan(ctx, st, p, actions, remote, nil, false) // scoped/delta — merge blocks, don't clear
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
	globalIgnore, _ := e.dirs.LoadIgnore()
	ig := engine.NewIgnore(append(append([]string{}, globalIgnore...), p.Excludes...))
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
	s.DevIgnoresSeeded = true
	if err := e.dirs.SaveSettings(s); err != nil {
		slog.Warn("could not persist dev-ignore migration flag", "err", err)
	}
}

func (e *Engine) Run(ctx context.Context, pairs []Pair, onSync func(Pair, transfer.Stats)) error {
	e.onSync = onSync
	e.seedDevIgnores()        // before any watcher fires its first scan
	defer e.closeStoreFinal() // resident baseline cache lives only while running
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

	e.pollInterval = 15 * time.Second
	if e.PushAvailable() {
		e.pollInterval = 5 * time.Minute // push handles the common case; poll is a safety net
		go e.runPush(ctx)
	}
	e.status("Up to date")

	for _, p := range pairs {
		e.startWatcher(p)
	}
	go e.watchPause(ctx) // resume/pause at timed expiry and schedule boundaries
	<-ctx.Done()
	// Wait for in-flight sync passes before the deferred closeStoreFinal
	// closes the state DB under them; a Stop-then-Start cycle also can't
	// overlap two engines' watchers on the same folders and DB this way.
	e.drainWatchers(30 * time.Second)
	return nil
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
			}()
		}
	})
}
