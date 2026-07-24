# Nimbo mobile facade — binding contract

The source of truth for the Kotlin side of the gomobile boundary
(Nimbo-Android consumes the `.aar` built from this package with
`gomobile bind ./mobile`). Everything here is API: treat changes to any of it
as breaking.

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

## Sync folders

- **Call `SetBaseDir` before the first `AddSyncFolder`** (once storage
  permission is granted, e.g. `/storage/emulated/0/Nimbo`). `AddSyncFolder`
  refuses to run while the base dir would fall back to app-private storage —
  pairs bake in their absolute local path at creation and a later
  `SetBaseDir` does **not** re-point them. A base dir persisted by a previous
  session counts; you don't need to re-set it every launch.
- `AddSyncPair(localDir, remoteRoot)` takes an explicit local directory and
  is not guarded (you chose the path).

## JSON payloads

All collection-returning methods return a JSON **array** — `[]` when empty,
never `null`.

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
| `OnPairSynced` stats (transfer.Stats) | **PascalCase** (untagged) | `Downloaded`, `Uploaded`, `MkLocal`, `MkRemote`, `DelLocal`, `DelRemote`, `Moved`, `Conflicts`, `ConflictsIdentical`, `ConflictsResurrected`, `Failed` |
| `ConflictsJSON` (ConflictItem) | **PascalCase** (untagged) | `LocalDir`, `RemoteRoot`, `Path`, `Kind`, `LocalExists`, `RemoteExists`, `LocalSize`, `LocalMTime`, `RemoteSize`, `RemoteMTime` |
| `DiagnosticsJSON` (Diagnostic) | **PascalCase** (untagged) | `ServerURL`, `ServerVersion`, `Account`, `PushAvailable`, `PushConnected`, `PushSince`, `LastStatus`, `LastSyncAt` |
| `BrowseJSON` (webdav Entry) | **PascalCase** (untagged) | `Path`, `IsDir`, `Size`, `ETag`, `FileID`, `LastModified`, `ContentType`, `Checksums` |

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
