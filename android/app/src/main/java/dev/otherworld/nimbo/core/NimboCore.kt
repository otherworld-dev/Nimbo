/*
 * NimboCore.kt — the single bridge between the app and the Go core.
 *
 * Everything that touches dev.otherworld.mobile goes through here. Rules:
 *  - one Client per process, created in init() from Application.onCreate();
 *  - every Client call blocks, so every suspend fn body runs on Dispatchers.IO;
 *  - start/stop are serialised by a Mutex (the core errors on double-start and
 *    forbids concurrent stop/start);
 *  - engine state is published as never-null StateFlows that Compose can collect;
 *  - nothing here imports anything from ui/ or touches Android UI.
 */
package dev.otherworld.nimbo.core

import android.content.Context
import android.os.Build
import android.os.Environment
import android.util.Log
import dev.otherworld.mobile.Client
import dev.otherworld.mobile.LoginFlow
import dev.otherworld.mobile.Mobile
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.channels.BufferOverflow
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asSharedFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext
import kotlinx.serialization.decodeFromString
import java.io.File
import java.io.IOException

object NimboCore {

    const val DEFAULT_BASE_DIR_NAME = "Nimbo"

    private const val TAG = "NimboCore"

    /** setBaseDir attempts before giving up (see setBaseDirWithRetry). */
    private const val BASE_DIR_ATTEMPTS = 3
    private const val BASE_DIR_RETRY_MS = 200L

    // -- process-wide state --------------------------------------------------

    @Volatile
    private var client: Client? = null

    /** Failure from the one-shot newClient() call, surfaced by startEngine(). */
    @Volatile
    private var initError: Throwable? = null

    /** Serialises start/stop: the core forbids double-start and concurrent stop/start. */
    private val lifecycleMutex = Mutex()

    /** Used only for fire-and-forget refreshes triggered by Go-thread callbacks. */
    internal val bridgeScope = CoroutineScope(SupervisorJob() + Dispatchers.IO)

    // -- observable engine state (written by EngineListener, read by UI/service) --

    internal val statusState = MutableStateFlow("")
    internal val progressState = MutableStateFlow<SyncProgress?>(null)
    internal val pausedState = MutableStateFlow(false)
    internal val runningState = MutableStateFlow(false)
    internal val pairsState = MutableStateFlow<List<SyncPair>>(emptyList())
    internal val conflictsChangedState = MutableStateFlow(0L)
    internal val notificationCountState = MutableStateFlow(0L)
    internal val lastPairSyncedState = MutableStateFlow<PairSyncedEvent?>(null)
    internal val lastSyncAtState = MutableStateFlow(0L)
    internal val authLostState = MutableStateFlow(false)
    internal val toastsState = MutableSharedFlow<ToastEvent>(
        replay = 0,
        extraBufferCapacity = 8,
        onBufferOverflow = BufferOverflow.DROP_OLDEST,
    )

    /** Last status line reported by the engine ("" until the first callback). */
    val status: StateFlow<String> = statusState.asStateFlow()

    /** Latest sync progress, or null when the engine has not reported any yet. */
    val progress: StateFlow<SyncProgress?> = progressState.asStateFlow()

    val paused: StateFlow<Boolean> = pausedState.asStateFlow()

    val running: StateFlow<Boolean> = runningState.asStateFlow()

    val pairs: StateFlow<List<SyncPair>> = pairsState.asStateFlow()

    /** Bumped counter — the UI re-fetches conflicts when it changes. */
    val conflictsChanged: StateFlow<Long> = conflictsChangedState.asStateFlow()

    val notificationCount: StateFlow<Long> = notificationCountState.asStateFlow()

    val toasts: SharedFlow<ToastEvent> = toastsState.asSharedFlow()

    /** The last pass that actually moved something (no-op polls do not replace it). */
    val lastPairSynced: StateFlow<PairSyncedEvent?> = lastPairSyncedState.asStateFlow()

    /** When any pass last completed, 0 if none yet — "checked at", not "transferred at". */
    val lastSyncAt: StateFlow<Long> = lastSyncAtState.asStateFlow()

    /** True once the core reports the session is gone; clearAuthLost() resets it. */
    val authLost: StateFlow<Boolean> = authLostState.asStateFlow()

    fun clearAuthLost() {
        authLostState.value = false
    }

    // -- setup ---------------------------------------------------------------

    /** The default sync root: <external storage>/Nimbo. */
    fun defaultBaseDir(): String =
        File(Environment.getExternalStorageDirectory(), DEFAULT_BASE_DIR_NAME).absolutePath

    /**
     * Creates the one Client for this process. Idempotent, cheap and offline —
     * safe to call from Application.onCreate(). A failure is remembered and
     * reported by startEngine() (and by any other call) rather than thrown here.
     */
    fun init(context: Context) {
        if (client != null) return
        synchronized(this) {
            if (client != null) return
            val appContext = context.applicationContext
            runCatching {
                Mobile.newClient(appContext.filesDir.absolutePath, KeystoreSecretStore(appContext))
            }.onSuccess { created ->
                client = created
                initError = null
                runCatching { runningState.value = created.isRunning() }
            }.onFailure { e ->
                initError = e
                Log.e(TAG, "newClient failed", e)
            }
        }
    }

    // -- engine lifecycle ----------------------------------------------------

    /**
     * Starts the engine. Returns success immediately when it is already running.
     * On success the base dir is ensured (best effort — its failure never fails
     * the start) and the pair list is refreshed.
     */
    suspend fun startEngine(): Result<Unit> = withContext(Dispatchers.IO) {
        lifecycleMutex.withLock {
            val c = client
            if (c == null) {
                return@withLock Result.failure<Unit>(notInitialised())
            }
            val alreadyRunning = runCatching { c.isRunning() }.getOrDefault(false)
            if (alreadyRunning) {
                runningState.value = true
                return@withLock Result.success(Unit)
            }
            val started = runCatching { c.start(EngineListener) }
            val failure = started.exceptionOrNull()
            if (failure != null) {
                Log.e(TAG, "start failed", failure)
                runningState.value = false
                return@withLock Result.failure<Unit>(failure)
            }
            runningState.value = true

            // Best effort from here on — none of it may fail the start.
            if (hasExternalStorageAccess()) {
                runCatching { ensureBaseDirBlocking(c) }
                    .onFailure { Log.w(TAG, "ensureBaseDir failed (ignored)", it) }
            }
            refreshPairsBlocking(c)
            runCatching { pausedState.value = c.isPaused() }
                .onFailure { Log.w(TAG, "isPaused failed (ignored)", it) }

            Result.success(Unit)
        }
    }

    /** Stops the engine and waits for it to drain. Safe to call when not running. */
    suspend fun stopEngine() {
        withContext(Dispatchers.IO) {
            lifecycleMutex.withLock {
                val c = client
                if (c != null) {
                    runCatching { c.stop() }.onFailure { Log.w(TAG, "stop failed", it) }
                }
                runningState.value = false
            }
        }
    }

    /** Cheap, non-blocking check straight from the core. */
    fun isRunning(): Boolean =
        runCatching { client?.isRunning() ?: false }.getOrDefault(false)

    // -- accounts (usable without the engine running) ------------------------

    fun hasAccount(): Boolean =
        runCatching { client?.hasAccount() ?: false }.getOrDefault(false)

    suspend fun accounts(): Result<List<NcAccount>> = io {
        parseList<NcAccount>(requireClient().accountsJSON())
    }

    /**
     * Begins a Login Flow v2. Open [LoginHandle.url] in a browser, then call
     * [LoginHandle.poll].
     */
    suspend fun startLogin(serverUrl: String): Result<LoginHandle> = io {
        val flow = requireClient().startLogin(serverUrl)
        LoginHandle(flow, flow.url() ?: "")
    }

    suspend fun logout(accountId: String): Result<Unit> = io {
        requireClient().logout(accountId)
    }

    // -- folders (require the engine) ----------------------------------------

    /** Creates <external>/Nimbo if needed and points the core at it. */
    suspend fun ensureBaseDir(): Result<String> = io {
        ensureBaseDirBlocking(requireClient())
    }

    suspend fun baseDir(): Result<String> = io {
        requireClient().baseDir() ?: ""
    }

    suspend fun addSyncFolder(remoteRoot: String): Result<Unit> = io {
        val c = requireClient()
        c.addSyncFolder(remoteRoot)
        refreshPairsBlocking(c)
    }

    /**
     * Binds an explicit local directory to a remote folder — the path the user
     * chose rather than the one under the base dir. Unlike [addSyncFolder] the
     * core does not guard this one (we picked the path), so we create the
     * directory ourselves first and fail loudly if we cannot.
     */
    suspend fun addSyncPair(localDir: String, remoteRoot: String): Result<Unit> = io {
        val c = requireClient()
        val dir = File(localDir)
        if (!dir.isDirectory && !dir.mkdirs()) {
            throw IOException("could not create $localDir")
        }
        c.addSyncPair(localDir, remoteRoot)
        refreshPairsBlocking(c)
    }

    /** Points the core's sync root at [dir], creating it if needed. */
    suspend fun setBaseDir(dir: String): Result<String> = io {
        val file = File(dir)
        if (!file.isDirectory && !file.mkdirs()) {
            throw IOException("could not create base directory $dir")
        }
        setBaseDirWithRetry(requireClient(), dir)
        dir
    }

    suspend fun removeSyncFolder(remoteRoot: String, deleteLocal: Boolean): Result<Unit> = io {
        val c = requireClient()
        c.removeSyncFolder(remoteRoot, deleteLocal)
        refreshPairsBlocking(c)
    }

    /** Re-reads the pair list into [pairs]. Never throws. */
    suspend fun refreshPairs() {
        withContext(Dispatchers.IO) {
            runCatching { refreshPairsBlocking(requireClient()) }
                .onFailure { Log.w(TAG, "pairs refresh failed", it) }
        }
    }

    /** Lists a remote directory ("" is the account root). */
    suspend fun browse(remotePath: String): Result<List<BrowseEntry>> = io {
        parseList<BrowseEntry>(requireClient().browseJSON(remotePath))
    }

    /** Pending conflicts; empty under the v1 auto policy. */
    suspend fun conflicts(): Result<List<ConflictItem>> = io {
        parseList<ConflictItem>(requireClient().conflictsJSON())
    }

    // -- trash ----------------------------------------------------------------

    /**
     * The server trashbin. An empty list means empty OR the trashbin app is
     * disabled server-side — the two are indistinguishable here.
     */
    suspend fun trash(): Result<List<TrashItem>> = io {
        parseList<TrashItem>(requireClient().trashJSON())
    }

    /**
     * Finds files and folders by NAME across the whole account. Hits carry real
     * paths, so they open like any other row — but a name search is not a
     * content search, and nothing may present it as one.
     */
    suspend fun search(term: String, limit: Int = 50): Result<List<BrowseEntry>> = io {
        parseList<BrowseEntry>(requireClient().searchJSON(term, limit.toLong()))
    }

    /**
     * Previous revisions of a file, by its oc:fileid. An empty list means the
     * file has no versions OR the server's versions app is off — indistinguishable
     * from here, so nothing may claim the file has never changed.
     */
    suspend fun versions(fileId: String): Result<List<FileVersion>> = io {
        parseList<FileVersion>(requireClient().versionsJSON(fileId))
    }

    /** Makes a previous revision current. The present contents become a version in turn. */
    suspend fun restoreVersion(href: String): Result<Unit> = io {
        requireClient().restoreVersion(href)
    }

    /**
     * Every share this account takes part in. The one core call returning an
     * object rather than an array, so it is parsed directly rather than through
     * [parseList].
     */
    suspend fun shares(): Result<SharesPayload> = io {
        NimboJson.decodeFromString<SharesPayload>(requireClient().sharesJSON())
    }

    /**
     * Forces a fresh read of the server's notifications.
     *
     * [notifications] serves the engine's cache, which is refilled from a push
     * event — and a push channel that is connected but silent never refills it.
     * Anything that must not miss a notification calls this first.
     */
    suspend fun refreshNotifications(): Result<Unit> = io {
        requireClient().refreshNotifications()
    }

    /** The account's server notifications. */
    suspend fun notifications(): Result<List<NcNotification>> = io {
        parseList<NcNotification>(requireClient().notificationsJSON())
    }

    /** Clears one notification. Server-side, so it clears on every signed-in device. */
    suspend fun dismissNotification(id: Int): Result<Unit> = io {
        requireClient().dismissNotification(id.toLong())
    }

    /** Clears every notification. No undo — they are deleted, not archived. */
    suspend fun dismissAllNotifications(): Result<Unit> = io {
        requireClient().dismissAllNotifications()
    }

    /** Runs an action the notification offered, with the server's own link and method. */
    suspend fun doNotificationAction(action: NcNotificationAction): Result<Unit> = io {
        requireClient().doNotificationAction(action.link, action.type)
    }

    /** The shares that exist on one path — "who can see this?" for a single file. */
    suspend fun sharesOn(remotePath: String): Result<List<Share>> = io {
        parseList<Share>(requireClient().sharesOnJSON(remotePath))
    }

    /**
     * Publishes a path behind a public link and returns the new share, whose
     * [Share.url] is the link. A server that requires link passwords refuses
     * outright rather than creating an open one — that error must reach the user.
     */
    suspend fun createPublicLink(
        remotePath: String,
        password: String = "",
        expiration: String = "",
    ): Result<Share> = io {
        NimboJson.decodeFromString<Share>(
            requireClient().createPublicLinkJSON(remotePath, password, expiration)
        )
    }

    /** Shares a path with another user on this server. Read-only. */
    suspend fun createUserShare(remotePath: String, user: String): Result<Share> = io {
        NimboJson.decodeFromString<Share>(
            requireClient().createUserShareJSON(remotePath, user, 0L)
        )
    }

    /** Revokes one share. The file itself is untouched — this removes access only. */
    suspend fun deleteShare(id: String): Result<Unit> = io {
        requireClient().deleteShare(id)
    }

    /** The user's starred files and folders. Same shape as [browse], so rows render identically. */
    suspend fun favorites(): Result<List<BrowseEntry>> = io {
        parseList<BrowseEntry>(requireClient().favoritesJSON())
    }

    /** Stars or unstars one path. The account root cannot be starred; the core refuses it. */
    suspend fun setFavorite(remotePath: String, favorite: Boolean): Result<Unit> = io {
        requireClient().setFavorite(remotePath, favorite)
    }

    /** Puts an item back. It returns to the server; a synced pair pulls it down next pass. */
    suspend fun restoreTrash(href: String): Result<Unit> = io {
        requireClient().restoreTrash(href)
    }

    /** Removes one item from the trashbin permanently — no further undo. */
    suspend fun deleteTrashItem(href: String): Result<Unit> = io {
        requireClient().deleteTrashItem(href)
    }

    // -- damage guard --------------------------------------------------------

    /**
     * Folders the engine has paused because a pass would have destroyed most of
     * them. Empty is the normal case.
     */
    suspend fun frozenFolders(): Result<List<FrozenFolder>> = io {
        parseList<FrozenFolder>(requireClient().frozenFoldersJSON())
    }

    /**
     * Resumes one paused folder. Errors when it is not actually paused, so a
     * stale list cannot silently resume a healthy folder — callers re-read
     * [frozenFolders] rather than assuming.
     */
    suspend fun clearFreeze(localDir: String): Result<Unit> = io {
        requireClient().clearFreeze(localDir)
    }

    /**
     * Folders whose last sync pass failed. Empty is healthy — and while it is
     * NOT empty the UI must not report the account as up to date.
     */
    suspend fun failingFolders(): Result<List<FailingFolder>> = io {
        parseList<FailingFolder>(requireClient().failingFoldersJSON())
    }

    // -- file management (the browser) ---------------------------------------

    /** Metadata for one remote path; fails when it does not exist. */
    suspend fun stat(remotePath: String): Result<BrowseEntry> = io {
        NimboJson.decodeFromString<BrowseEntry>(requireClient().statJSON(remotePath) ?: "{}")
    }

    /**
     * Server-rendered thumbnail bytes, or a failure when the file is not
     * previewable — which is ordinary, not an error worth showing the user.
     * NOTE: the generated binding takes a Long.
     */
    suspend fun preview(fileId: String, px: Int): Result<ByteArray> = io {
        requireClient().previewJPEG(fileId, px.toLong())
    }

    /**
     * Streams a remote file to [localPath]. No deadline (see MOBILE_API.md) — a
     * large file on a slow link would trip any fixed ceiling, so call this from
     * somewhere a long block is acceptable.
     */
    suspend fun downloadToFile(remotePath: String, localPath: String): Result<String> = io {
        requireClient().downloadToFile(remotePath, localPath)
        localPath
    }

    /** Uploads a local file to [remotePath], creating parent collections. */
    suspend fun uploadFile(localPath: String, remotePath: String): Result<Unit> = io {
        requireClient().uploadFile(localPath, remotePath)
    }

    suspend fun mkdirRemote(remotePath: String): Result<Unit> = io {
        requireClient().mkdirRemote(remotePath)
    }

    suspend fun deleteRemote(remotePath: String): Result<Unit> = io {
        requireClient().deleteRemote(remotePath)
    }

    /** Moves or renames a remote path. */
    suspend fun moveRemote(src: String, dst: String): Result<Unit> = io {
        requireClient().moveRemote(src, dst)
    }

    // -- control + info (require the engine) ---------------------------------

    suspend fun syncNow(): Result<Unit> = io {
        requireClient().syncNow()
    }

    suspend fun setPaused(paused: Boolean): Result<Unit> = io {
        requireClient().setPaused(paused)
        pausedState.value = paused
    }

    suspend fun quota(): Result<Quota> = io {
        NimboJson.decodeFromString<Quota>(requireClient().quotaJSON() ?: "{}")
    }

    suspend fun apps(): Result<List<NcApp>> = io {
        parseList<NcApp>(requireClient().appsJSON())
    }

    suspend fun diagnostics(): Result<Diagnostics> = io {
        NimboJson.decodeFromString<Diagnostics>(requireClient().diagnosticsJSON() ?: "{}")
    }

    suspend fun serverUrl(): Result<String> = io {
        requireClient().serverURL() ?: ""
    }

    /**
     * The appearance the user enabled in Nextcloud: "dark", "light", or
     * "default" (they follow their own OS). Costs a live request, so this is
     * read when the theme is resolved rather than continuously.
     */
    suspend fun themeAppearance(): Result<String> = io {
        requireClient().themeAppearance() ?: ""
    }

    suspend fun themeColor(): Result<String> = io {
        requireClient().themeColor() ?: ""
    }

    // -- internals -----------------------------------------------------------

    /** Called from EngineListener.onPauseChanged (a Go thread): re-read off-thread. */
    internal fun refreshPausedAsync() {
        bridgeScope.launch {
            runCatching { pausedState.value = requireClient().isPaused() }
                .onFailure { Log.w(TAG, "isPaused refresh failed", it) }
        }
    }

    /** Every blocking call funnels through here: IO dispatcher + Result. */
    private suspend inline fun <T> io(crossinline body: () -> T): Result<T> =
        withContext(Dispatchers.IO) { runCatching { body() } }

    private inline fun <reified T> parseList(json: String?): List<T> {
        val text = json?.trim().orEmpty()
        if (text.isEmpty() || text == "null") return emptyList()
        return NimboJson.decodeFromString(text)
    }

    private fun requireClient(): Client =
        client ?: throw notInitialised()

    private fun notInitialised(): IllegalStateException {
        val cause = initError
        return if (cause != null) {
            IllegalStateException("Nimbo core failed to start: ${cause.message}", cause)
        } else {
            IllegalStateException("Nimbo core is not initialised")
        }
    }

    /** Blocking pair refresh. Swallows its own failures — callers treat it as best effort. */
    private fun refreshPairsBlocking(c: Client) {
        runCatching { pairsState.value = parseList<SyncPair>(c.pairsJSON()) }
            .onFailure { Log.w(TAG, "pairs refresh failed (ignored)", it) }
    }

    /** Blocking half of ensureBaseDir — reused by startEngine inside the mutex. */
    private fun ensureBaseDirBlocking(c: Client): String {
        val dir = defaultBaseDir()
        val file = File(dir)
        if (!file.isDirectory && !file.mkdirs()) {
            throw IOException("could not create base directory $dir")
        }
        setBaseDirWithRetry(c, dir)
        return dir
    }

    /**
     * setBaseDir, retried and then verified.
     *
     * The core persists settings by writing a fixed "settings.json.tmp" and
     * renaming it. When one of its own saves races ours — which is exactly what
     * happens when we set the base dir immediately after Start — the first
     * rename consumes the temp file and the second fails with ENOENT. Observed
     * on device, so: retry, and confirm the value actually landed rather than
     * trusting a silent success.
     */
    private fun setBaseDirWithRetry(c: Client, dir: String) {
        var last: Throwable? = null
        repeat(BASE_DIR_ATTEMPTS) { attempt ->
            val applied = runCatching {
                c.setBaseDir(dir)
                c.baseDir()
            }
            val value = applied.getOrNull()
            if (applied.isSuccess && value == dir) return
            last = applied.exceptionOrNull()
                ?: IllegalStateException("base dir did not take effect (core reports \"$value\")")
            Log.w(TAG, "setBaseDir attempt ${attempt + 1} failed", last)
            if (attempt < BASE_DIR_ATTEMPTS - 1) {
                // We are on Dispatchers.IO; blocking briefly here is fine.
                runCatching { Thread.sleep(BASE_DIR_RETRY_MS) }
            }
        }
        throw last ?: IllegalStateException("could not set base directory $dir")
    }

    private fun hasExternalStorageAccess(): Boolean =
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
            Environment.isExternalStorageManager()
        } else {
            // Below API 30 the legacy storage permission covers us; if it is not
            // granted mkdirs() simply fails and the step is skipped anyway.
            true
        }
}

/**
 * A live Login Flow v2 attempt. Open [url] in a Custom Tab, then [poll] — it
 * blocks (on IO) until the user approves, the flow times out (10 minutes) or
 * [cancel] aborts it.
 */
class LoginHandle internal constructor(
    private val flow: LoginFlow,
    val url: String,
) {
    suspend fun poll(): Result<NcAccount> = withContext(Dispatchers.IO) {
        runCatching {
            val account = flow.poll()
            NcAccount(
                id = account.getID() ?: "",
                serverURL = account.getServerURL() ?: "",
                loginName = account.getLoginName() ?: "",
            )
        }
    }

    fun cancel() {
        runCatching { flow.cancel() }
    }
}
