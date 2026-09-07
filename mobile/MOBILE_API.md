# Nimbo mobile facade — binding contract

The source of truth for the Kotlin side of the gomobile boundary. The consumer
lives in this same repo at [`android/`](../android), and builds the `.aar` from
this package via `android/scripts/build-core.ps1` (`gomobile bind ./mobile`).
Everything here is API: treat changes to any of it as breaking — and because
both sides are now one tree, change them in one commit.

## Lifecycle

- **`NewClient(rootDir, secrets)` — call once per process**, from
  `Application.onCreate()`, with `Context.getFilesDir()` as `rootDir`. The
  first call configures the process (config/data dirs, the secret store);
  later calls return a fresh `Client` bound to that same setup and **ignore
  their arguments**. Keep one singleton.
- **`Start(listener)`** blocks on the network (capabilities fetch) — call it
  from a background thread. It **errors if the engine is already running**:
  to swap listeners, `Stop()` first, let it return, then `Start` again. Never
  run `Stop` and `Start` concurrently from different threads.
- **`Stop()`** returns once the run loop has exited **and in-flight pair
  syncs have drained** (bounded at 30 seconds for a wedged pass, matching the
  engine's stop convention), so a subsequent `Start` cannot overlap the old
  engine's syncs or its state database.
- A theoretical race remains if `AddSyncFolder`/`AddSyncPair` lands in the
  same instant `Start`'s engine boots: the pair is **recorded but not yet
  watched** (never a crash), and its watcher starts on the next pair change.
  In practice unreachable from UI-driven calls.
- Every method that talks to the server or filesystem blocks — call from
  `Dispatchers.IO`.

## Listener rules

- Callbacks arrive on **Go-owned threads** — hop to the main thread before
  touching UI.
- **Callbacks must never throw.** gomobile generates no exception check for
  void callback methods, so a Kotlin exception escaping `onStatus`,
  `onProgress`, etc. kills the whole process (pending JNI exception). Wrap
  every handler body in `try/catch`.
- `OnToast` carries **engine-generated notices only** (sync errors,
  conflicts, blocked files). Server notifications (shares, mentions, …)
  arrive via `OnNotificationsChanged` → fetch `NotificationsJSON`.

## SecretStore rules (Keystore adapter)

- `Get` returns `""` for "no secret stored" — **absence must not be an
  error**. App passwords are never empty, so `""` is unambiguous.
- `Delete` must **succeed when the entry is already absent** (treat
  "not found" as success). Logout is retried against missing aliases after
  backup restores; an exception here would make an account unremovable.
- No method may throw (same JNI rule as listeners).

## Login flow

`StartLogin(serverURL)` → open `URL()` in a Custom Tab → `Poll()` from a
background thread.

- `serverURL` is normalised for you (whitespace trimmed, `https://` default
  scheme, trailing slash dropped) — pass raw user input.
- `Poll` **retries transient network errors internally** (Wi-Fi→cellular
  handovers are survivable); an error from `Poll` is terminal for that flow.
  The whole flow times out after 10 minutes. `Cancel()` aborts an in-flight
  `Poll`.
- A `LoginFlow` you constructed yourself (gomobile emits a no-arg
  constructor) is inert: `URL()` returns `""`, `Poll()` errors, `Cancel()`
  no-ops. Only `StartLogin` produces a usable flow.

## Sync control

- `SyncNow` runs a **full reconcile** of every pair: both trees are walked, so
  local-only files upload and remote-only files download. It is deliberately not
  the cheap remote-delta poll the desktop tray fires — Android's inotify does not
  reliably report writes made by *other* apps to shared storage, so this is the
  user's manual recovery path when the watcher missed a change. Expect it to take
  as long as a startup sync on a large pair, and call it off the main thread like
  everything else.
- A pair's **first** pass reconciles both sides. It begins with a bulk download
  clone, then — when the local folder already had content — continues into the
  normal diff in the same pass, so pre-existing local files are uploaded rather
  than ignored. (Before this, pairing a populated folder with an empty remote
  uploaded nothing and still reported "Up to date".)

## Sync folders

- **Call `SetBaseDir` before the first `AddSyncFolder`** (once storage
  permission is granted, e.g. `/storage/emulated/0/Nimbo`). `AddSyncFolder`
  refuses to run while the base dir would fall back to app-private storage —
  pairs bake in their absolute local path at creation and a later
  `SetBaseDir` does **not** re-point them. A base dir persisted by a previous
  session counts; you don't need to re-set it every launch.
- `AddSyncPair(localDir, remoteRoot)` takes an explicit local directory and
  is not guarded (you chose the path).

## File management

The file-browser surface. All of these need the engine running.

- `StatJSON(path)` returns one `Entry` object (not an array) and **errors when the
  path does not exist** — absence is not an empty result here.
- `PreviewJPEG(fileID, px)` returns server-rendered thumbnail bytes for images,
  PDFs and office documents, at most `px` per side (`0` = 256). An error means
  "not previewable" far more often than it means a fault — show a type icon and
  do not report it to the user.
- `DownloadToFile(remotePath, localPath)` and `UploadFile(localPath, remotePath)`
  stream, create parent directories/collections, and **carry no overall
  deadline** — any fixed ceiling would break a large file on a slow link. The
  transport still bounds the connection phases, so a dead server cannot hang them
  forever, but there is no way to cancel one except to abandon the thread. Run
  them where that is acceptable (a foreground service, not the UI scope).
  A failed download leaves no partial file behind.
- `MkdirRemote(path)`, `DeleteRemote(path)`, `MoveRemote(src, dst)` are ordinary
  30-second operations. `MoveRemote` covers both rename and move.
- `DeleteRemote` is only recoverable if the server has its trashbin enabled.
  Present it as permanent unless you have checked.

None of these touch the sync baseline: deleting or moving a path inside a synced
pair changes the server, and the next sync pass then propagates that to the local
copy like any other remote change.

## Damage guard

The engine pauses ("freezes") a sync folder rather than applying a pass that
would delete or replace at least 50 files and at least half of what it knows
about, or when a server listing comes back empty or sharply shrunk. The freeze
survives restarts and state-database resets.

- The freeze is announced through **`OnToast`** — no separate callback — so an
  app that surfaces toasts already tells the user.
- `FrozenFoldersJSON()` lists what is paused: `[{localDir, reason, sample}]`,
  `[]` when nothing is. `sample` carries a few affected paths so the user can
  judge whether the change was theirs.
- `ClearFreeze(localDir)` resumes one folder, granting a single-pass exemption.
  It **errors when that folder is not actually paused**, so a stale list cannot
  silently "resume" a healthy folder — re-read `FrozenFoldersJSON` after any
  failure rather than assuming.

This matters more on Android than on desktop: a pair living on shared storage
looks exactly like a mass deletion when the volume fails to mount or the
All-files-access permission is withdrawn. Without a resume path in the app, that
folder stays paused until the user finds a desktop.

## Per-folder health

`OnStatus` is per-ACCOUNT, not per-folder. When one folder syncs happily and
another cannot, the healthy one's "Up to date" is what the listener receives —
so a UI built only on the status string tells the user everything is fine while
a folder has stopped syncing entirely. This was observed on Android: a pair
whose storage permission was withdrawn failed every pass, raised one alert
notification, and then looked perfectly healthy in the app.

- `FailingFoldersJSON()` returns `[{localDir, lastError, since}]`, `[]` when all
  folders are healthy. `since` is RFC 3339 (when the folder STARTED failing, not
  the latest attempt) or `""` if unknown.
- **Do not render "Up to date" while that list is non-empty.** Show the failure
  against the folder it belongs to.
- The entry clears itself on the folder's next successful pass, and is dropped
  when the pair is removed.

Distinct from `FrozenFoldersJSON`: a freeze is a deliberate pause awaiting
review; this is simply "the last pass errored". A folder can be in both lists.

## Trash

- `TrashJSON()` lists the server trashbin. `Href` is the handle for the other
  two calls — opaque, do not construct or parse it.
- `RestoreTrash(href)` puts an item back where it was deleted from. It returns
  to the SERVER; a synced pair pulls it down on the next pass like any other
  remote change, so the local copy does not reappear instantly.
- `DeleteTrashItem(href)` is permanent. There is no further undo.

An empty list means the trashbin is empty **or** the server has the trashbin app
disabled — the two are indistinguishable here, so do not tell the user "nothing
has been deleted" on the strength of it.

## Favourites, search, shares and versions

- `FavoritesJSON()` returns starred files/folders as `Entry` — the same shape
  as `BrowseJSON`, so the same row renderer works. Paths are account-relative
  and can be opened directly.
- `SetFavorite(remotePath, fav)` stars or unstars one path. `BrowseJSON` now
  reports the current state as `Entry.IsFavorite`, so a star can be drawn
  filled before the user touches it. Favouriting the account root is refused.
- `SearchJSON(term, limit)` finds files and folders whose **name** contains
  term, anywhere in the account, returning `Entry` — the same shape as
  `BrowseJSON`, with real account-relative paths, so a hit opens like any
  other row. Backed by WebDAV `SEARCH`, not the unified-search provider,
  precisely so the hits carry paths.
  Names only: the server's unified search can reach file *contents* where it
  indexes them and this cannot, so do not present it as a full-text search.
  A blank term is an error, not a match-everything.
- `SharesJSON()` returns a JSON **object**, not an array:
  `{"own": [...], "received": [...]}`. `own` is what the user shared out;
  `received` is what was shared with them. A received share's `path` is the
  path in the OWNER'S account and need not exist in the user's own tree — do
  not feed it to `BrowseJSON` unchecked.
- `SharesOnJSON(remotePath)` lists the shares on ONE path — what a
  "who can see this?" view for a single file reads.
- `CreatePublicLinkJSON(remotePath, password, expiration)` publishes a path
  behind a link and returns the new `Share`, whose `url` is the link — so a
  client need not re-list to find what it just made. `password` may be empty,
  but a server configured to require one refuses the whole request rather than
  creating an open link: **show that error, never swallow it**.
  `expiration` is `YYYY-MM-DD` or empty.
- `CreateUserShareJSON(remotePath, user, permissions)` shares with another user
  on this server; `permissions` of 0 means read-only.
- `DeleteShare(id)` revokes one share. **The file is untouched** — this removes
  access, it does not delete anything.

Sharing the **account root is refused** by all three creation calls: `""` and
`"/"` both resolve to everything the user owns, and one mistyped path should
not be able to publish an entire account behind a single link. A blank share id
is likewise refused, since it would aim a DELETE at the shares collection
rather than at one share.

- `VersionsJSON(fileID)` lists previous revisions of a file by its `oc:fileid`
  (`Entry.FileID`), newest first. `RestoreVersion(href)` makes one current; the
  file's present contents become a version in turn.
  An empty array means no prior versions **or** the versions app is disabled.

## Notifications

- `NotificationsJSON()` lists the account's server notifications. **This is a
  cache**, not a live read.
- `RefreshNotifications()` re-fetches it from the server and fires
  `OnNotificationsChanged`. Needed because the engine refills that cache from a
  notify_push event, and its post-sync fallback runs only when push is
  *unavailable* — so a push channel that is connected but silent leaves the
  cache stale indefinitely. Poll this if your client must not miss anything. The
  `OnNotificationsChanged(count)` listener fires when the set changes — that
  callback carries only a count, so re-fetch the list to see what changed.
- `DismissNotification(id)` clears one by its `notification_id`.
- `DismissAllNotifications()` clears every one.
- `DoNotificationAction(link, method)` runs an action the notification
  itself offered (Accept / Decline / …). Both values must come **verbatim**
  from that notification's `actions` array — never construct them. An empty
  method means GET.

Dismissal happens on the **server**, so it applies to every device the account
is signed in to, and there is no undo: notifications are deleted, not archived.

## Theming

- `ThemeColor()` returns the server's theming colour (e.g. `#0082c9`) from
  cached capabilities. Use it as the UI accent, as the desktop client does.
- `ThemeAppearance()` returns `dark`, `light` or `default` — the appearance the
  user enabled in Nextcloud. **Costs a live HTTP request** (no capability
  advertises it; it is read from the web UI's markup), so call it on demand,
  not per frame. Resolve `default` against the device's own setting.

Neither is a brand colour: they are the user's Nextcloud, and a client should
fall back to its own palette when there is no account yet or the call fails.

## JSON payloads

All collection-returning methods return a JSON **array** — `[]` when empty,
never `null`. The one exception is `SharesJSON`, which returns an object of two
such arrays (see above).

Field naming is pinned per payload (changing any of these breaks the shipped
app — there is no compile-time signal across the boundary):

| Payload | Naming | Fields |
|---|---|---|
| `ProgressJSON` / `OnProgress` (SyncProgress) | camelCase | `active`, `current`, `done`, `total`, `speed`, `avgSpeed`, `doneBytes`, `totalBytes`, `enumerating` |
| `PairsJSON` (SyncPair) | camelCase | `localDir`, `remoteRoot`, `excludes` |
| `AccountsJSON` (Account) | camelCase | `id`, `serverURL`, `loginName` |
| `QuotaJSON` (QuotaInfo) | camelCase | `free`, `used`, `total`, `relative`, `quota` |
| `AppsJSON` (App) | camelCase | `id`, `name`, `href`, `icon` |
| `NotificationsJSON` (Notification) | server-style | `notification_id`, `app`, `subject`, `message`, `link`, `object_type`, `datetime`, `actions` |
| `actions[]` (NotificationAction) | camelCase | `label`, `link`, `type` (HTTP method), `primary` |
| `OnPairSynced` stats (transfer.Stats) | **PascalCase** (untagged) | `Downloaded`, `Uploaded`, `MkLocal`, `MkRemote`, `DelLocal`, `DelRemote`, `Moved`, `Conflicts`, `ConflictsIdentical`, `ConflictsResurrected`, `Failed` |
| `TrashJSON` (TrashItem) | **PascalCase** (untagged) | `Href`, `Name`, `OriginalLocation`, `DeletedAt`, `Size`, `IsDir` |
| `ConflictsJSON` (ConflictItem) | **PascalCase** (untagged) | `LocalDir`, `RemoteRoot`, `Path`, `Kind`, `LocalExists`, `RemoteExists`, `LocalSize`, `LocalMTime`, `RemoteSize`, `RemoteMTime` |
| `DiagnosticsJSON` (Diagnostic) | **PascalCase** (untagged) | `ServerURL`, `ServerVersion`, `Account`, `PushAvailable`, `PushConnected`, `PushSince`, `LastStatus`, `LastSyncAt` |
| `FrozenFoldersJSON` | camelCase | `localDir`, `reason`, `sample` |
| `FailingFoldersJSON` | camelCase | `localDir`, `lastError`, `since` |
| `BrowseJSON` / `FavoritesJSON` / `SearchJSON` (webdav Entry) | **PascalCase** (untagged) | `Path`, `IsDir`, `Size`, `ETag`, `FileID`, `LastModified`, `ContentType`, `Checksums`, `IsFavorite` |
| `SharesJSON` (object of Share arrays) | camelCase | `own[]`, `received[]`; each: `id`, `share_type`, `item_type` (`file`/`folder`), `path`, `permissions`, `share_with`, `url`, `token`, `expiration`, `uid_owner`, `displayname_owner` |
| `VersionsJSON` (FileVersion) | **PascalCase** (untagged) | `Href`, `Modified`, `Size` |

Untagged `time.Time` fields serialise as RFC 3339 strings. Model the
PascalCase payloads as-is in Kotlin (`@SerialName` per field); do not expect
them to be normalised later — the same structs feed the desktop GUI, so
retagging them is not on the table.

## Error signalling

Go error identity does not cross gomobile — Kotlin sees message strings only.
The one string worth matching today: `"no app password stored for account"`
from `Start` means the account exists but its secret is gone (e.g. Keystore
didn't survive a device-to-device restore) → route to sign-in. A typed
`NeedsLogin()` API is tracked follow-up work.
