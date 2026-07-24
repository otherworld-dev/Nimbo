// Package mobile is the gomobile facade over Nimbo's sync engine — the API
// surface the Android app binds against (via `gomobile bind`, producing an
// .aar consumed by the Nimbo-Android repo).
//
// gomobile restricts exported signatures to primitives, strings, []byte,
// error, bound structs, and interfaces, so collections cross the boundary as
// JSON strings and events arrive through the Listener interface implemented
// in Kotlin.
//
// Threading: every method that talks to the server or filesystem blocks and
// must be called off the Android main thread (a coroutine on Dispatchers.IO).
// Listener callbacks arrive on arbitrary Go-owned threads — hop to the main
// thread before touching UI.
package mobile

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/otherworld/nimbo/internal/account"
	"github.com/otherworld/nimbo/internal/agent"
	"github.com/otherworld/nimbo/internal/config"
	"github.com/otherworld/nimbo/internal/notify"
	"github.com/otherworld/nimbo/internal/transfer"
)

// SecretStore is implemented in Kotlin (Android Keystore / EncryptedSharedPreferences)
// and holds app passwords.
//
// Contract:
//   - Get returns "" when no secret is stored — app passwords are never
//     empty, so the empty string is unambiguous. Absence must NOT be
//     reported as an error (a generic error here is indistinguishable from a
//     real Keystore fault and blocks the sign-in prompt).
//   - Delete must succeed (and do nothing) when no secret is stored: treat
//     "not found" as success, never as an exception — logout retries depend
//     on it.
//   - Methods must not throw: an exception crossing the gomobile boundary
//     kills the process.
type SecretStore interface {
	Get(accountID string) (string, error)
	Set(accountID, secret string) error
	Delete(accountID string) error
}

// Listener receives engine events. Implemented in Kotlin; methods are invoked
// from Go-owned threads — hop to the main thread before touching UI.
//
// Implementations MUST NOT throw: gomobile generates no exception check for
// void callbacks, so a Kotlin exception escaping any of these methods kills
// the whole process. Wrap handler bodies in try/catch.
type Listener interface {
	// OnStatus reports the engine's human-readable state ("Up to date",
	// "Syncing…", …) — mirror it into the foreground-service notification.
	OnStatus(status string)
	// OnProgress carries an agent.SyncProgress snapshot as JSON.
	OnProgress(progressJSON string)
	// OnToast is an engine-generated notice (sync error, conflict, blocked
	// file) to surface as an Android notification. link may be empty. Server
	// notifications (shares, mentions, …) do NOT arrive here — watch
	// OnNotificationsChanged and fetch NotificationsJSON.
	OnToast(title, message, link string)
	// OnAuthLost fires when the server rejects our credentials (revoked app
	// password) — prompt the user to sign in again.
	OnAuthLost()
	// OnPairSynced fires after a pair finishes a sync pass; statsJSON is the
	// transfer.Stats for the pass.
	OnPairSynced(localDir, remoteRoot, statsJSON string)
	// OnNotificationsChanged fires when the server-side notification list
	// changes; count is the current number.
	OnNotificationsChanged(count int)
	// OnConflictsChanged fires when the pending-conflict set changes; fetch it
	// with ConflictsJSON.
	OnConflictsChanged()
	// OnPauseChanged fires when the effective pause state flips.
	OnPauseChanged()
}

// secretAdapter maps the Kotlin-friendly SecretStore ("" = absent) onto the
// engine's account.SecretStore (ErrNoSecret sentinel).
type secretAdapter struct{ s SecretStore }

func (a secretAdapter) Get(id string) (string, error) {
	v, err := a.s.Get(id)
	if err != nil {
		return "", err
	}
	if v == "" {
		return "", account.ErrNoSecret
	}
	return v, nil
}
func (a secretAdapter) Set(id, secret string) error { return a.s.Set(id, secret) }
func (a secretAdapter) Delete(id string) error      { return a.s.Delete(id) }

// Client is the root object: one per process, created by NewClient.
type Client struct {
	mu         sync.Mutex
	rootDir    string
	engine     *agent.Engine
	stop       func() // cancels the engine and waits for its run loop to exit
	starting   bool   // Start is building the engine (outside the lock)
	baseDirSet bool   // SetBaseDir succeeded this session
}

// setupMu guards the one-time process setup performed by the first NewClient
// call. Later calls reuse it (their rootDir/secrets are ignored), so an
// Android service recreation can safely construct a fresh Client without
// racing os.Setenv/SetSecretStore against a still-draining engine.
var (
	setupMu   sync.Mutex
	setupDone bool
)

// NewClient prepares Nimbo to run out of rootDir (the app's private files
// directory, Context.getFilesDir()) with app passwords kept in secrets. The
// first call configures the process; later calls return a fresh Client bound
// to that same setup.
func NewClient(rootDir string, secrets SecretStore) (*Client, error) {
	if rootDir == "" {
		return nil, errors.New("rootDir is required")
	}
	if secrets == nil {
		return nil, errors.New("secret store is required")
	}
	setupMu.Lock()
	defer setupMu.Unlock()
	if !setupDone {
		// The engine resolves its config/data dirs via os.UserConfigDir and the
		// XDG variables (GOOS=android takes the unix code path), and Android app
		// processes set none of them — point everything into rootDir.
		if err := os.MkdirAll(rootDir, 0o700); err != nil {
			return nil, err
		}
		os.Setenv("HOME", rootDir)
		os.Setenv("XDG_CONFIG_HOME", filepath.Join(rootDir, "config"))
		os.Setenv("XDG_DATA_HOME", filepath.Join(rootDir, "data"))
		account.SetSecretStore(secretAdapter{secrets})
		// No desktop toasts on Android — notifications flow through Listener.
		notify.SetEnabled(false)
		setupDone = true
	}
	return &Client{rootDir: rootDir}, nil
}

// ---- Login (Nextcloud Login Flow v2) ----

// LoginFlow is an in-progress browser login started by StartLogin.
type LoginFlow struct {
	flow   *account.Flow
	ctx    context.Context
	cancel context.CancelFunc
}

// StartLogin begins Login Flow v2 against serverURL. Open URL() in a Custom
// Tab, then call Poll from a background thread; the flow times out after ten
// minutes.
func (c *Client) StartLogin(serverURL string) (*LoginFlow, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	f, err := account.InitLogin(ctx, serverURL)
	if err != nil {
		cancel()
		return nil, err
	}
	return &LoginFlow{flow: f, ctx: ctx, cancel: cancel}, nil
}

// URL is the page the user must open in a browser to approve the login.
// Empty on a LoginFlow that did not come from StartLogin (gomobile generates a
// public no-arg constructor, so zero-value instances can reach us from Kotlin).
func (l *LoginFlow) URL() string {
	if l == nil || l.flow == nil {
		return ""
	}
	return l.flow.LoginURL
}

// Poll blocks until the user approves the login (or the flow is cancelled /
// times out), then persists the account — its app password goes to the
// SecretStore — and makes it the active account. Transient network errors are
// retried internally, so an error here is terminal for this flow.
func (l *LoginFlow) Poll() (*Account, error) {
	if l == nil || l.flow == nil {
		return nil, errors.New("login flow not started — use Client.StartLogin")
	}
	defer l.cancel()
	creds, err := l.flow.Poll(l.ctx)
	if err != nil {
		return nil, err
	}
	d, err := config.Resolve()
	if err != nil {
		return nil, err
	}
	st, err := account.LoadStore(d.AccountsFile())
	if err != nil {
		return nil, err
	}
	acc, err := account.Complete(st, creds)
	if err != nil {
		return nil, err
	}
	return &Account{ID: acc.ID, ServerURL: acc.ServerURL, LoginName: acc.LoginName}, nil
}

// Cancel aborts an in-progress Poll. Safe on a zero-value LoginFlow.
func (l *LoginFlow) Cancel() {
	if l != nil && l.cancel != nil {
		l.cancel()
	}
}

// Account is a configured Nextcloud login (no secret material).
type Account struct {
	ID        string
	ServerURL string
	LoginName string
}

// HasAccount reports whether at least one account is configured.
func (c *Client) HasAccount() bool {
	d, err := config.Resolve()
	if err != nil {
		return false
	}
	st, err := account.LoadStore(d.AccountsFile())
	if err != nil {
		return false
	}
	_, ok := st.Default()
	return ok
}

// AccountsJSON returns the configured accounts as a JSON array of
// {id, serverURL, loginName}.
func (c *Client) AccountsJSON() (string, error) {
	d, err := config.Resolve()
	if err != nil {
		return "", err
	}
	st, err := account.LoadStore(d.AccountsFile())
	if err != nil {
		return "", err
	}
	return marshalSlice(st.Accounts)
}

// Logout removes the account, then best-effort deletes its stored app
// password: a secret-store failure (Keystore quirks after a backup restore)
// must never leave an unremovable account, matching the desktop sign-out.
// Stop the engine first if it is running for this account.
func (c *Client) Logout(accountID string) error {
	d, err := config.Resolve()
	if err != nil {
		return err
	}
	st, err := account.LoadStore(d.AccountsFile())
	if err != nil {
		return err
	}
	if err := st.Remove(accountID); err != nil {
		return err
	}
	_ = account.DeleteSecret(accountID)
	return nil
}

// ---- Engine lifecycle ----

// Start brings the sync engine up for the active account and begins watching
// every configured pair. It fetches server capabilities, so it blocks on the
// network; run it from the foreground service on a background thread. Events
// arrive on l until Stop. Returns an error if the engine is already running —
// Stop first, then Start with the new listener (a second Start can never
// silently rewire callbacks).
func (c *Client) Start(l Listener) error {
	if l == nil {
		return errors.New("listener is required")
	}
	c.mu.Lock()
	if c.engine != nil || c.starting {
		c.mu.Unlock()
		return errors.New("engine already running — call Stop before Start to change listeners")
	}
	c.starting = true
	c.mu.Unlock()
	ok := false
	defer func() {
		if !ok {
			c.mu.Lock()
			c.starting = false
			c.mu.Unlock()
		}
	}()

	// Build the engine outside c.mu: NewEngineFor talks to the server, and
	// holding the lock across the network would wedge every other method
	// (Stop included) behind a slow or stalled connection.
	ctx, cancel := context.WithCancel(context.Background())
	e, err := agent.NewEngineFor(ctx, "")
	if err != nil {
		cancel()
		return err
	}
	// v1 resolves conflicts automatically (keep both on true divergence) so no
	// conflict is ever blocking; a conflict UI can switch this to PolicyAsk.
	e.SetConflictPolicy(transfer.PolicyAuto)
	e.SetStatusFunc(l.OnStatus)
	e.SetProgressFunc(func(p agent.SyncProgress) {
		if b, err := json.Marshal(p); err == nil {
			l.OnProgress(string(b))
		}
	})
	e.SetToastFunc(l.OnToast)
	e.SetAuthLostFunc(l.OnAuthLost)
	e.SetPauseChangeFunc(l.OnPauseChanged)

	// Subscribe before Run starts: the engine's initial notification fetch
	// signals subscribers, and a channel created only after that signal
	// misses it — the badge would stay empty until the next brand-new
	// server notification.
	nsub := e.Notifier().Subscribe()
	csub := e.SubscribeConflicts()
	go func() { // forward server-notification and conflict changes
		for {
			select {
			case <-ctx.Done():
				return
			case <-nsub:
				l.OnNotificationsChanged(e.Notifier().Count())
			case <-csub:
				l.OnConflictsChanged()
			}
		}
	}()

	pairs, err := e.Pairs()
	if err != nil {
		cancel()
		return err
	}
	aps := make([]agent.Pair, 0, len(pairs))
	for _, p := range pairs {
		aps = append(aps, agent.Pair{LocalDir: p.LocalDir, RemoteRoot: p.RemoteRoot, Excludes: p.Excludes})
	}

	done := make(chan struct{})
	started := make(chan struct{})
	go func() {
		defer close(done)
		// Signal that this goroutine is executing before entering Run, so an
		// AddSyncFolder immediately after Start almost always finds the run
		// context published. If one still wins the race, the engine records
		// the pair without watching it (no panic) and the next pair change
		// starts its watcher.
		close(started)
		_ = e.Run(ctx, aps, func(p agent.Pair, s transfer.Stats) {
			if b, err := json.Marshal(s); err == nil {
				l.OnPairSynced(p.LocalDir, p.RemoteRoot, string(b))
			}
		})
	}()
	<-started

	c.mu.Lock()
	c.engine = e
	c.stop = func() { cancel(); <-done }
	c.starting = false
	c.mu.Unlock()
	ok = true
	return nil
}

// Stop shuts the sync engine down; safe to call when not running. It returns
// once the run loop has exited and in-flight pair syncs have drained (bounded
// at 30s for a wedged pass), so a subsequent Start cannot overlap the old
// engine's syncs. Let Stop return before calling Start rather than running
// the two concurrently.
func (c *Client) Stop() {
	c.mu.Lock()
	stop := c.stop
	c.engine, c.stop = nil, nil
	c.mu.Unlock()
	if stop != nil {
		stop() // outside c.mu: listener callbacks may re-enter Client methods
	}
}

// IsRunning reports whether the engine is up.
func (c *Client) IsRunning() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.engine != nil
}

func (c *Client) eng() (*agent.Engine, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.engine == nil {
		return nil, errors.New("engine not running — call Start first")
	}
	return c.engine, nil
}

// opCtx bounds one-off server operations.
func opCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

func marshal(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// marshalSlice is marshal for slices, encoding nil as [] — the documented
// contract for the *JSON methods is a JSON array, and Kotlin's
// JSONArray("null") throws.
func marshalSlice[T any](s []T) (string, error) {
	if s == nil {
		s = []T{}
	}
	return marshal(s)
}

// ---- Sync control ----

// SyncNow triggers an immediate sync pass on every pair.
func (c *Client) SyncNow() error {
	e, err := c.eng()
	if err != nil {
		return err
	}
	e.TriggerSync()
	return nil
}

// SetPaused pauses (true) or resumes (false) syncing.
func (c *Client) SetPaused(p bool) error {
	e, err := c.eng()
	if err != nil {
		return err
	}
	e.SetPaused(p)
	return nil
}

// IsPaused reports the effective pause state.
func (c *Client) IsPaused() (bool, error) {
	e, err := c.eng()
	if err != nil {
		return false, err
	}
	return e.Paused(), nil
}

// ---- Sync folders ----

// BaseDir is the local root under which selected remote folders are synced
// (e.g. /storage/emulated/0/Nimbo). Set it once storage permission is granted.
func (c *Client) BaseDir() (string, error) {
	e, err := c.eng()
	if err != nil {
		return "", err
	}
	return e.BaseDir(), nil
}

// SetBaseDir changes the local sync root for folders added via AddSyncFolder
// (e.g. /storage/emulated/0/Nimbo). Call it once storage permission is
// granted, before the first AddSyncFolder.
func (c *Client) SetBaseDir(dir string) error {
	e, err := c.eng()
	if err != nil {
		return err
	}
	if err := e.SetBaseDir(dir); err != nil {
		return err
	}
	c.mu.Lock()
	c.baseDirSet = true
	c.mu.Unlock()
	return nil
}

// PairsJSON returns the configured sync pairs as a JSON array of
// {localDir, remoteRoot, excludes}.
func (c *Client) PairsJSON() (string, error) {
	e, err := c.eng()
	if err != nil {
		return "", err
	}
	pairs, err := e.Pairs()
	if err != nil {
		return "", err
	}
	return marshalSlice(pairs)
}

// AddSyncFolder selects a remote folder for sync under BaseDir and starts
// watching it immediately. It refuses to run before SetBaseDir (a base dir
// persisted by a previous session counts): the engine's fallback base dir
// lives inside the app-private rootDir, which no file manager can see and
// Android deletes on uninstall — and pairs keep their baked-in path even if
// the base dir is fixed later.
func (c *Client) AddSyncFolder(remoteRoot string) error {
	e, err := c.eng()
	if err != nil {
		return err
	}
	c.mu.Lock()
	explicit := c.baseDirSet
	c.mu.Unlock()
	if !explicit && isUnder(e.BaseDir(), c.rootDir) {
		return errors.New("no sync base directory configured — call SetBaseDir before AddSyncFolder, or files would sync into app-private storage")
	}
	return e.AddSyncFolder(remoteRoot)
}

// isUnder reports whether path is root or lives inside it.
func isUnder(path, root string) bool {
	if root == "" {
		return false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// AddSyncPair binds an explicit local directory to a remote folder (e.g. the
// camera roll to Photos/Camera) and starts watching it immediately.
func (c *Client) AddSyncPair(localDir, remoteRoot string) error {
	e, err := c.eng()
	if err != nil {
		return err
	}
	return e.AddSyncPair(localDir, remoteRoot)
}

// RemoveSyncFolder stops syncing the remote folder; deleteLocal also removes
// the local copy.
func (c *Client) RemoveSyncFolder(remoteRoot string, deleteLocal bool) error {
	e, err := c.eng()
	if err != nil {
		return err
	}
	return e.RemoveSyncFolder(remoteRoot, deleteLocal)
}

// BrowseJSON lists a remote directory (for the folder picker) as a JSON array
// of WebDAV entries. Path "" or "/" is the account root.
func (c *Client) BrowseJSON(remotePath string) (string, error) {
	e, err := c.eng()
	if err != nil {
		return "", err
	}
	ctx, cancel := opCtx()
	defer cancel()
	entries, err := e.Browse(ctx, remotePath)
	if err != nil {
		return "", err
	}
	return marshalSlice(entries)
}

// ---- Status & server info ----

// ProgressJSON returns the live agent.SyncProgress snapshot.
func (c *Client) ProgressJSON() (string, error) {
	e, err := c.eng()
	if err != nil {
		return "", err
	}
	return marshal(e.Progress())
}

// DiagnosticsJSON returns engine health (push connectivity, last sync, …).
func (c *Client) DiagnosticsJSON() (string, error) {
	e, err := c.eng()
	if err != nil {
		return "", err
	}
	return marshal(e.Diagnostics())
}

// ConflictsJSON returns pending conflicts (empty under the v1 auto policy).
func (c *Client) ConflictsJSON() (string, error) {
	e, err := c.eng()
	if err != nil {
		return "", err
	}
	return marshalSlice(e.PendingConflicts())
}

// NotificationsJSON returns current server notifications.
func (c *Client) NotificationsJSON() (string, error) {
	e, err := c.eng()
	if err != nil {
		return "", err
	}
	return marshalSlice(e.Notifier().List())
}

// QuotaJSON returns the account's storage quota.
func (c *Client) QuotaJSON() (string, error) {
	e, err := c.eng()
	if err != nil {
		return "", err
	}
	ctx, cancel := opCtx()
	defer cancel()
	q, err := e.Quota(ctx)
	if err != nil {
		return "", err
	}
	return marshal(q)
}

// AppsJSON returns the server's navigation apps (id, name, icon, href) — the
// source for the "Nextcloud apps as native apps" launcher.
func (c *Client) AppsJSON() (string, error) {
	e, err := c.eng()
	if err != nil {
		return "", err
	}
	ctx, cancel := opCtx()
	defer cancel()
	apps, err := e.Apps(ctx)
	if err != nil {
		return "", err
	}
	return marshalSlice(apps)
}

// ServerURL returns the active account's server base URL.
func (c *Client) ServerURL() (string, error) {
	e, err := c.eng()
	if err != nil {
		return "", err
	}
	return e.ServerURL(), nil
}

// ThemeColor returns the server's theming colour (e.g. "#0082c9").
func (c *Client) ThemeColor() (string, error) {
	e, err := c.eng()
	if err != nil {
		return "", err
	}
	return e.ThemeColor(), nil
}
