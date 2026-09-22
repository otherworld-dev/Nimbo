// Package mobile is the gomobile facade over Nimbo's sync engine — the API
// surface the Android app binds against (via `gomobile bind`, producing an
// .aar consumed by android/ in this same repo).
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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/otherworld/nimbo/internal/account"
	"github.com/otherworld/nimbo/internal/agent"
	"github.com/otherworld/nimbo/internal/config"
	"github.com/otherworld/nimbo/internal/notify"
	"github.com/otherworld/nimbo/internal/transfer"
	"github.com/otherworld/nimbo/internal/transport"
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
	acc, err := account.Complete(d.AccountsFile(), creds)
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
	if err := account.Update(d.AccountsFile(), func(st *account.Store) error {
		st.Remove(accountID)
		return nil
	}); err != nil {
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

// SyncNow triggers an immediate FULL reconcile of every pair — both trees are
// walked, so local-only changes upload and remote-only changes download.
//
// Deliberately not the cheaper remote-delta pass that the desktop tray's "Sync
// now" fires: on Android, inotify does not reliably report writes made by OTHER
// apps to shared storage, so this is the user's manual recovery path when the
// watcher missed something. A "Sync now" that only polls the server could never
// upload a file the watcher failed to see, which is exactly the complaint it
// exists to answer.
func (c *Client) SyncNow() error {
	e, err := c.eng()
	if err != nil {
		return err
	}
	e.TriggerFullSync()
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

// ---- File management (the Android file browser) ----

// StatJSON returns one remote entry's metadata as a JSON object, or an error if
// it does not exist.
func (c *Client) StatJSON(remotePath string) (string, error) {
	e, err := c.eng()
	if err != nil {
		return "", err
	}
	ctx, cancel := opCtx()
	defer cancel()
	entry, ok, err := e.StatRemote(ctx, remotePath)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errors.New("not found: " + remotePath)
	}
	return marshal(entry)
}

// PreviewJPEG returns a server-rendered thumbnail for a file id, at most px
// pixels per side (0 picks a sensible default). Images, PDFs and office
// documents all answer; anything else returns an error, which the caller should
// treat as "show a type icon" rather than as a failure worth reporting.
func (c *Client) PreviewJPEG(fileID string, px int) ([]byte, error) {
	e, err := c.eng()
	if err != nil {
		return nil, err
	}
	ctx, cancel := opCtx()
	defer cancel()
	return e.Preview(ctx, fileID, px)
}

// DownloadToFile streams a remote file to localPath, creating parent
// directories and leaving nothing behind if it fails.
//
// Deliberately has no overall deadline: a large file over a slow link would trip
// any fixed ceiling. The transport still bounds the connection phases, so a dead
// server cannot hang this forever — but a caller that wants to give up early
// must do so by abandoning the thread it is blocking.
func (c *Client) DownloadToFile(remotePath, localPath string) error {
	e, err := c.eng()
	if err != nil {
		return err
	}
	return e.DownloadTo(context.Background(), remotePath, localPath)
}

// UploadFile pushes a local file to remotePath, creating parent collections.
// Chunked for large files. Same deadline reasoning as DownloadToFile.
func (c *Client) UploadFile(localPath, remotePath string) error {
	e, err := c.eng()
	if err != nil {
		return err
	}
	_, uerr := e.Upload(context.Background(), localPath, remotePath)
	return uerr
}

// MkdirRemote creates a remote folder (parents included).
func (c *Client) MkdirRemote(remotePath string) error {
	e, err := c.eng()
	if err != nil {
		return err
	}
	ctx, cancel := opCtx()
	defer cancel()
	return e.MkdirRemote(ctx, remotePath)
}

// DeleteRemote removes a remote file or folder. On a server with the trashbin
// enabled this is recoverable; treat it as permanent in the UI unless you have
// checked.
func (c *Client) DeleteRemote(remotePath string) error {
	e, err := c.eng()
	if err != nil {
		return err
	}
	ctx, cancel := opCtx()
	defer cancel()
	return e.DeleteRemote(ctx, remotePath)
}

// MoveRemote moves or renames a remote path.
func (c *Client) MoveRemote(src, dst string) error {
	e, err := c.eng()
	if err != nil {
		return err
	}
	ctx, cancel := opCtx()
	defer cancel()
	return e.MoveRemote(ctx, src, dst)
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

// ---- Favourites, search and versions ----

// FavoritesJSON lists the user's starred files and folders as a JSON array of
// Entry (PascalCase, untagged) — the same shape BrowseJSON returns, so a client
// can render it with the file-row code it already has.
//
// The paths are account-relative and can be opened directly; unlike search
// hits, a favourite always knows where it lives.
func (c *Client) FavoritesJSON() (string, error) {
	e, err := c.eng()
	if err != nil {
		return "", err
	}
	ctx, cancel := opCtx()
	defer cancel()
	entries, err := e.Favorites(ctx)
	if err != nil {
		return "", err
	}
	return marshalSlice(entries)
}

// SetFavorite stars (fav = true) or unstars a file or folder by its
// account-relative path. Starring the account root is refused.
//
// BrowseJSON reports the current state as Entry.IsFavorite, so a client can
// show the star filled before the user touches it.
func (c *Client) SetFavorite(remotePath string, fav bool) error {
	e, err := c.eng()
	if err != nil {
		return err
	}
	ctx, cancel := opCtx()
	defer cancel()
	return e.SetFavorite(ctx, remotePath, fav)
}

// SharesOnJSON lists the shares that exist on ONE path, as a JSON array of
// Share. This is what a "who can see this?" view for a single file reads.
func (c *Client) SharesOnJSON(remotePath string) (string, error) {
	e, err := c.eng()
	if err != nil {
		return "", err
	}
	ctx, cancel := opCtx()
	defer cancel()
	shares, err := e.ListShares(ctx, remotePath)
	if err != nil {
		return "", err
	}
	return marshalSlice(shares)
}

// CreatePublicLinkJSON publishes remotePath behind a public link and returns
// the new Share as JSON — its "url" is the link, available immediately so the
// caller need not re-list to find what it just made.
//
// password may be empty, but a server configured to require one will refuse
// the whole request rather than create an open link; the error says so and
// must be shown, not swallowed. expiration is "YYYY-MM-DD" or empty for none.
//
// Sharing the account root is refused.
func (c *Client) CreatePublicLinkJSON(remotePath, password, expiration string) (string, error) {
	e, err := c.eng()
	if err != nil {
		return "", err
	}
	ctx, cancel := opCtx()
	defer cancel()
	share, err := e.CreatePublicLink(ctx, remotePath, transport.PublicLinkOptions{
		Password:   password,
		Expiration: expiration,
	})
	if err != nil {
		return "", err
	}
	return marshal(share)
}

// CreateUserShareJSON shares remotePath with another user on this server and
// returns the new Share as JSON. permissions of 0 means read-only.
//
// The user must exist on the server; a wrong username is refused by the server
// rather than silently creating a share nobody holds.
func (c *Client) CreateUserShareJSON(remotePath, user string, permissions int) (string, error) {
	e, err := c.eng()
	if err != nil {
		return "", err
	}
	ctx, cancel := opCtx()
	defer cancel()
	share, err := e.CreateUserShare(ctx, remotePath, user, permissions)
	if err != nil {
		return "", err
	}
	return marshal(share)
}

// DeleteShare revokes one share by its id. The FILE is untouched — this takes
// away access, it does not delete anything.
func (c *Client) DeleteShare(id string) error {
	e, err := c.eng()
	if err != nil {
		return err
	}
	ctx, cancel := opCtx()
	defer cancel()
	return e.DeleteShare(ctx, id)
}

// SharesJSON returns every share this account takes part in, as a JSON OBJECT
// (not an array) with two keys, each an array of Share (camelCase, tagged):
//
//	{"own": [...], "received": [...]}
//
// "own" is what the user shared out; "received" is what was shared with them.
// They are kept apart because the two mean opposite things to a user and a
// single merged list cannot say which is which.
//
// Share.path is account-relative for "own" shares. For a RECEIVED share it is
// the path in the OWNER'S account, which need not exist in the user's own tree
// — do not feed it to BrowseJSON without checking.
func (c *Client) SharesJSON() (string, error) {
	e, err := c.eng()
	if err != nil {
		return "", err
	}
	ctx, cancel := opCtx()
	defer cancel()
	own, received, err := e.Shares(ctx)
	if err != nil {
		return "", err
	}
	if own == nil {
		own = []transport.Share{}
	}
	if received == nil {
		received = []transport.Share{}
	}
	return marshal(struct {
		Own      []transport.Share `json:"own"`
		Received []transport.Share `json:"received"`
	}{own, received})
}

// SearchJSON finds files and folders whose NAME contains term, anywhere in the
// account, returning up to limit of them as a JSON array of Entry — the same
// shape BrowseJSON returns, so hits render and open like any other row.
//
// Names only. The server's unified search can reach file CONTENTS where it
// indexes them; this cannot, and a client should not imply otherwise.
//
// A blank term is an error rather than a match-everything.
func (c *Client) SearchJSON(term string, limit int) (string, error) {
	e, err := c.eng()
	if err != nil {
		return "", err
	}
	ctx, cancel := opCtx()
	defer cancel()
	hits, err := e.SearchByName(ctx, term, limit)
	if err != nil {
		return "", err
	}
	return marshalSlice(hits)
}

// VersionsJSON lists the stored previous revisions of a file by its oc:fileid
// (Entry.FileID from BrowseJSON) as a JSON array of FileVersion (PascalCase,
// untagged): Href, Modified, Size. Newest first.
//
// An empty array means the file has no prior versions OR the server's versions
// app is off — the two look the same from here.
func (c *Client) VersionsJSON(fileID string) (string, error) {
	e, err := c.eng()
	if err != nil {
		return "", err
	}
	ctx, cancel := opCtx()
	defer cancel()
	versions, err := e.Versions(ctx, fileID)
	if err != nil {
		return "", err
	}
	return marshalSlice(versions)
}

// RestoreVersion makes a previous revision the current one, by the Href from
// VersionsJSON. The file's present contents become a version in turn, so this
// is reversible — but only while the versions app keeps them.
func (c *Client) RestoreVersion(versionHref string) error {
	e, err := c.eng()
	if err != nil {
		return err
	}
	ctx, cancel := opCtx()
	defer cancel()
	return e.RestoreVersion(ctx, versionHref)
}

// ---- Trash ----

// TrashJSON lists the server trashbin as a JSON array of TrashItem
// (PascalCase, untagged): Href, Name, OriginalLocation, DeletedAt, Size, IsDir.
//
// Href is the handle for RestoreTrash and DeleteTrashItem — treat it as opaque.
// Empty when the trashbin is empty OR when the server has the app disabled, so
// an empty list is not evidence that nothing was deleted.
func (c *Client) TrashJSON() (string, error) {
	e, err := c.eng()
	if err != nil {
		return "", err
	}
	ctx, cancel := opCtx()
	defer cancel()
	items, err := e.Trash(ctx)
	if err != nil {
		return "", err
	}
	return marshalSlice(items)
}

// RestoreTrash puts a trashed item back where it came from. The file returns to
// the server; a synced pair then pulls it down on the next pass like any other
// remote change.
func (c *Client) RestoreTrash(href string) error {
	e, err := c.eng()
	if err != nil {
		return err
	}
	ctx, cancel := opCtx()
	defer cancel()
	return e.RestoreTrash(ctx, href)
}

// DeleteTrashItem removes one item from the trashbin permanently. There is no
// second chance after this — present it accordingly.
func (c *Client) DeleteTrashItem(href string) error {
	e, err := c.eng()
	if err != nil {
		return err
	}
	ctx, cancel := opCtx()
	defer cancel()
	return e.DeleteTrash(ctx, href)
}

// ---- Per-folder health ----

// folderHealth is one sync folder whose last pass failed.
type folderHealth struct {
	LocalDir  string `json:"localDir"`
	LastError string `json:"lastError"`
	Since     string `json:"since"`
}

// FailingFoldersJSON lists the sync folders whose last pass failed, as a JSON
// array of {localDir, lastError, since}; `[]` when everything is healthy.
//
// This exists because the engine's status line is per-ACCOUNT, not per-folder:
// one healthy folder reporting "Up to date" masks another that has stopped
// syncing completely. A UI that shows only the status string will tell the user
// everything is fine while nothing reaches the server — so show these per
// folder, and do not claim "Up to date" while this list is non-empty.
//
// Distinct from FrozenFoldersJSON: a freeze is a deliberate pause awaiting
// review, this is simply "the last pass errored" (offline, permission lost,
// folder unmounted). A folder can be in both.
func (c *Client) FailingFoldersJSON() (string, error) {
	e, err := c.eng()
	if err != nil {
		return "", err
	}
	healths := e.PairHealths()
	out := make([]folderHealth, 0, len(healths))
	for localDir, h := range healths {
		if !h.Failing {
			continue
		}
		since := ""
		if !h.Since.IsZero() {
			since = h.Since.Format(time.RFC3339)
		}
		out = append(out, folderHealth{LocalDir: localDir, LastError: h.LastError, Since: since})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LocalDir < out[j].LocalDir })
	return marshalSlice(out)
}

// ---- Damage guard ----

// frozenFolder is one sync folder the damage guard has paused. Flattened from
// the engine's map so the payload is a JSON array like every other collection.
type frozenFolder struct {
	LocalDir string   `json:"localDir"`
	Reason   string   `json:"reason"`
	Sample   []string `json:"sample"`
}

// FrozenFoldersJSON lists the folders the damage guard has paused, as a JSON
// array of {localDir, reason, sample}.
//
// The guard pauses a folder rather than applying a pass that would delete or
// replace a large share of it — the signature of a vanished mount, a revoked
// storage permission, or server-side ransomware. On Android a folder that lives
// on shared storage can hit this simply because the permission was withdrawn, so
// the app must be able to show it and offer a way out.
//
// Empty when nothing is paused; sample carries a few affected paths so the user
// can judge whether the change was theirs.
func (c *Client) FrozenFoldersJSON() (string, error) {
	e, err := c.eng()
	if err != nil {
		return "", err
	}
	views := e.FrozenViews()
	out := make([]frozenFolder, 0, len(views))
	for localDir, v := range views {
		if !v.Frozen {
			continue
		}
		out = append(out, frozenFolder{
			LocalDir: localDir,
			Reason:   v.FreezeReason,
			Sample:   append([]string{}, v.FreezeSample...),
		})
	}
	// Stable order: the UI renders this as a list and a map's iteration order
	// would reshuffle it on every poll.
	sort.Slice(out, func(i, j int) bool { return out[i].LocalDir < out[j].LocalDir })
	return marshalSlice(out)
}

// ClearFreeze resumes a folder the guard paused, granting it a single-pass
// exemption. Errors when the folder is not actually paused, so a stale UI cannot
// silently "resume" a healthy folder.
func (c *Client) ClearFreeze(localDir string) error {
	e, err := c.eng()
	if err != nil {
		return err
	}
	return e.ClearFreeze(localDir)
}

// DismissNotification removes one notification by its notification_id. The
// notification is cleared on the SERVER, so it goes on every device the account
// is signed in to — not just this one.
//
// A notification that is already gone counts as success.
func (c *Client) DismissNotification(id int) error {
	e, err := c.eng()
	if err != nil {
		return err
	}
	ctx, cancel := opCtx()
	defer cancel()
	return e.DismissNotification(ctx, id)
}

// DismissAllNotifications clears every notification for the account, on the
// server and therefore everywhere. There is no undo: dismissed notifications
// are not archived, they are deleted.
func (c *Client) DismissAllNotifications() error {
	e, err := c.eng()
	if err != nil {
		return err
	}
	ctx, cancel := opCtx()
	defer cancel()
	return e.DismissAllNotifications(ctx)
}

// DoNotificationAction runs one of a notification's own actions (Accept,
// Decline, and so on) by the link and HTTP method the SERVER supplied with it.
//
// Both values must come verbatim from the notification's actions array —
// never construct them. method may be empty, which means GET.
func (c *Client) DoNotificationAction(link, method string) error {
	e, err := c.eng()
	if err != nil {
		return err
	}
	if strings.TrimSpace(link) == "" {
		return errors.New("notification action: no link")
	}
	ctx, cancel := opCtx()
	defer cancel()
	return e.DoNotificationAction(ctx, transport.NotificationAction{
		Link: link,
		Type: method,
	})
}

// RefreshNotifications re-fetches the notification list from the server and
// updates what NotificationsJSON returns, firing OnNotificationsChanged.
//
// This exists because NotificationsJSON serves a CACHE. The engine refills that
// cache from a notify_push "notify_notification" event, and its post-sync
// fallback runs only when push is unavailable — so a push channel that is
// connected but silent (a dropped websocket the client still believes in, a
// server not forwarding notification events) leaves the cache stale forever.
// A client that wants to be sure calls this.
//
// Does not toast: the caller is asking, so the engine should not also announce.
func (c *Client) RefreshNotifications() error {
	e, err := c.eng()
	if err != nil {
		return err
	}
	ctx, cancel := opCtx()
	defer cancel()
	return e.Notifier().Refresh(ctx)
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

// ThemeAppearance reports the appearance the user has enabled in Nextcloud:
// "dark", "light", or "default" (they follow their OS).
//
// Unlike ThemeColor this costs a live request — the server advertises no
// capability for it, so it is read from the web UI's own markup. Call it when
// the answer is needed, not on every frame.
//
// A client that honours this should still resolve "default" against the
// device's own setting; there is nothing else to follow.
func (c *Client) ThemeAppearance() (string, error) {
	e, err := c.eng()
	if err != nil {
		return "", err
	}
	ctx, cancel := opCtx()
	defer cancel()
	return e.ThemeAppearance(ctx)
}

// ThemeColor returns the server's theming colour (e.g. "#0082c9").
func (c *Client) ThemeColor() (string, error) {
	e, err := c.eng()
	if err != nil {
		return "", err
	}
	return e.ThemeColor(), nil
}
