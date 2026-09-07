/*
 * NimboViewModel.kt — the only place the UI talks to the engine.
 *
 * It mirrors every NimboCore flow into a single UiState, owns the sign-in flow
 * (start login -> hand the URL to the host's Custom Tab -> poll on a cancellable
 * coroutine), and turns user intents into NimboCore / SyncService calls. All
 * NimboCore suspend functions already hop to Dispatchers.IO internally; the two
 * non-suspend Client-backed helpers (hasAccount/isRunning) are still wrapped in
 * withContext(Dispatchers.IO) here so nothing JNI-shaped ever runs on the main
 * thread.
 */
package dev.otherworld.nimbo.ui

import android.app.Application
import android.net.Uri
import android.util.Log
import dev.otherworld.nimbo.ui.theme.AppearancePreference
import androidx.core.content.edit
import android.content.SharedPreferences
import androidx.lifecycle.AndroidViewModel
import androidx.lifecycle.viewModelScope
import dev.otherworld.nimbo.core.LoginHandle
import dev.otherworld.nimbo.core.BrowseEntry
import dev.otherworld.nimbo.core.FileRow
import dev.otherworld.nimbo.core.FileVersion
import dev.otherworld.nimbo.core.newestFirst
import dev.otherworld.nimbo.core.FilesRepository
import dev.otherworld.nimbo.core.NcNotification
import dev.otherworld.nimbo.core.NcNotificationAction
import dev.otherworld.nimbo.core.NimboCore
import dev.otherworld.nimbo.service.ServerNotifications
import dev.otherworld.nimbo.core.Share
import dev.otherworld.nimbo.core.TrashItem
import dev.otherworld.nimbo.platform.FileOpener
import dev.otherworld.nimbo.core.joinRemote
import dev.otherworld.nimbo.core.remoteNameProblem
import dev.otherworld.nimbo.core.renameTargetRemote
import dev.otherworld.nimbo.platform.StagedUpload
import dev.otherworld.nimbo.platform.Uploads
import dev.otherworld.nimbo.platform.LocalFs
import dev.otherworld.nimbo.platform.Permissions
import dev.otherworld.nimbo.service.SyncService
import dev.otherworld.nimbo.service.SyncWorker
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import kotlinx.coroutines.withTimeoutOrNull

private const val TAG = "NimboViewModel"

/** Ceiling on a UI-blocking engine start (see ensureEngine). */
private const val ENGINE_START_TIMEOUT_MS = 45_000L

class NimboViewModel(app: Application) : AndroidViewModel(app) {

    private companion object {
        const val PREFS_UI = "ui_prefs"
        const val KEY_APPEARANCE = "appearance"
    }


    // Seeded rather than set in init: the appearance decides the very first
    // frame's colour scheme, and an init block runs after this field exists but
    // still leaves one composition with the default.
    private val _state = MutableStateFlow(
        UiState(
            appearance = AppearancePreference.fromStored(
                runCatching {
                    app.getSharedPreferences(PREFS_UI, Application.MODE_PRIVATE)
                        .getString(KEY_APPEARANCE, null)
                }.getOrNull()
            )
        )
    )
    val state: StateFlow<UiState> = _state.asStateFlow()

    /** The in-flight login flow, if any. Cancelled by [cancelSignIn]. */
    private var loginHandle: LoginHandle? = null
    private var signInJob: Job? = null

    init {
        mirrorEngineFlows()
        decideStartRoute()
    }

    // ---------------------------------------------------------------- engine -> UI

    private fun mirrorEngineFlows() {
        viewModelScope.launch {
            NimboCore.status.collect { s -> _state.update { it.copy(status = s) } }
        }
        viewModelScope.launch {
            NimboCore.progress.collect { p -> _state.update { it.copy(progress = p) } }
        }
        viewModelScope.launch {
            NimboCore.paused.collect { p -> _state.update { it.copy(paused = p) } }
        }
        viewModelScope.launch {
            NimboCore.running.collect { r ->
                _state.update { it.copy(running = r) }
                if (r) {
                    // The engine just came up (usually from the foreground service):
                    // pull the things that only work while it is running.
                    NimboCore.refreshPairs()
                    loadQuotaQuietly()
                    refreshFrozen()
                    // The account's colours: the app should look like the
                    // user's server as soon as it can, not after they visit
                    // a settings screen.
                    loadTheme()
                    // Files is the tab the app opens on, and nothing else triggers
                    // its first load — without this the browser sits on an empty
                    // "Nothing here yet" until the user switches tabs and back.
                    if (_state.value.tab == Tab.FILES) browseFiles(_state.value.filesPath)
                }
            }
        }
        viewModelScope.launch {
            NimboCore.pairs.collect { p -> _state.update { it.copy(pairs = p) } }
        }
        viewModelScope.launch {
            NimboCore.lastPairSynced.collect { e -> _state.update { it.copy(lastSynced = e) } }
        }
        viewModelScope.launch {
            // The engine sends only a count; the list is fetched when opened.
            // The badge can therefore be right while the list is still stale,
            // which is the correct trade: the badge is what draws the eye.
            NimboCore.notificationCount.collect { n ->
                _state.update { it.copy(notificationCount = n.toInt()) }
            }
        }
        viewModelScope.launch {
            NimboCore.lastSyncAt.collect { at ->
                _state.update { it.copy(lastSyncAt = at) }
                // A pass just finished; its outcome may have changed which
                // folders are healthy.
                refreshFrozen()
            }
        }
        viewModelScope.launch {
            NimboCore.toasts.collect { t ->
                val text = when {
                    t.title.isBlank() -> t.message
                    t.message.isBlank() -> t.title
                    else -> "${t.title}: ${t.message}"
                }
                if (text.isNotBlank()) setMessage(text)
                // A damage-guard freeze is announced through OnToast and nothing
                // else, so this is the only signal that the paused set changed.
                refreshFrozen()
            }
        }
        viewModelScope.launch {
            NimboCore.authLost.collect { lost ->
                if (!lost) return@collect
                // Tear the engine down before asking for a new sign-in. A running
                // engine holds the app password it loaded at Start and never
                // re-reads it, and the core latches its auth-lost flag until a
                // request succeeds — so a re-login against a still-running engine
                // would rebind nothing, fire no further event, and leave sync
                // permanently dead while the UI claimed to be signed in.
                stopService()
                NimboCore.stopEngine()
                _state.update {
                    it.copy(
                        route = Route.SIGN_IN,
                        signingIn = false,
                        account = null,
                        running = false,
                        message = "Session expired — sign in again",
                    )
                }
                NimboCore.clearAuthLost()
            }
        }
    }

    private fun decideStartRoute() {
        viewModelScope.launch {
            refreshPermissions()
            val hasAccount = withContext(Dispatchers.IO) { NimboCore.hasAccount() }
            if (!hasAccount) {
                _state.update { it.copy(route = Route.SIGN_IN) }
                return@launch
            }
            loadAccount()
            if (!_state.value.hasAllFiles) {
                _state.update { it.copy(route = Route.PERMISSIONS) }
                return@launch
            }
            _state.update { it.copy(route = Route.HOME) }
            startService()
        }
    }

    private suspend fun loadAccount() {
        val account = NimboCore.accounts().getOrNull()?.firstOrNull()
        if (account != null) _state.update { it.copy(account = account) }
    }

    // ---------------------------------------------------------------- permissions

    /** Re-reads the special-access states. Called from the activity's ON_RESUME. */
    // ---------------------------------------------------------------- appearance

    /**
     * Loads the user's Nextcloud theme colour and appearance.
     *
     * Failures are silent by design: a theme is a nicety, and an account that
     * cannot report one should still get an app, in the app's own colours.
     */
    fun loadTheme() {
        viewModelScope.launch {
            if (!NimboCore.isRunning()) return@launch
            NimboCore.themeColor().onSuccess { hex ->
                _state.update { it.copy(themeColor = hex) }
            }
            // Costs a live request, so it is loaded here rather than polled.
            NimboCore.themeAppearance().onSuccess { appearance ->
                _state.update { it.copy(serverAppearance = appearance) }
            }
        }
    }

    fun openSettings() {
        _state.update { it.copy(route = Route.SETTINGS) }
        loadTheme()
    }

    /** Records the appearance choice and applies it immediately. */
    fun setAppearance(preference: AppearancePreference) {
        _state.update { it.copy(appearance = preference) }
        viewModelScope.launch {
            runCatching {
                prefs().edit { putString(KEY_APPEARANCE, preference.stored) }
            }.onFailure { Log.w(TAG, "could not save the appearance preference", it) }
        }
    }

    private fun prefs(): SharedPreferences =
        getApplication<Application>().getSharedPreferences(PREFS_UI, Application.MODE_PRIVATE)

    fun refreshPermissions() {
        val context = getApplication<Application>()
        _state.update {
            it.copy(
                hasAllFiles = Permissions.hasAllFilesAccess(),
                hasNotifications = Permissions.hasNotifications(context),
                ignoringBattery = Permissions.isIgnoringBatteryOptimizations(context),
            )
        }
    }

    fun onPermissionsContinue() {
        if (!_state.value.hasAllFiles) {
            setMessage("All files access is required before Nimbo can sync")
            return
        }
        _state.update { it.copy(route = Route.HOME) }
        startService()
    }

    // ---------------------------------------------------------------- sign in

    fun onServerUrlChange(v: String) {
        _state.update { it.copy(serverUrlInput = v, signInError = null) }
    }

    /**
     * Starts Login Flow v2. [openUrl] is supplied by the activity and opens the
     * returned URL in a Custom Tab. The poll blocks for up to ten minutes on the
     * IO dispatcher; the job is cancellable and [cancelSignIn] also aborts the
     * native flow so the blocking call actually returns.
     */
    fun signIn(openUrl: (String) -> Unit) {
        val serverUrl = _state.value.serverUrlInput.trim()
        if (serverUrl.isEmpty()) {
            _state.update { it.copy(signInError = "Enter your Nextcloud server address") }
            return
        }
        if (signInJob?.isActive == true) return

        _state.update { it.copy(signingIn = true, signInError = null) }
        signInJob = viewModelScope.launch {
            // Identity of this attempt, so a cancelled predecessor's finally block
            // cannot null out the handle/job belonging to the attempt that replaced it.
            val self = coroutineContext[Job]
            try {
                val handle = NimboCore.startLogin(serverUrl).getOrElse { error ->
                    _state.update {
                        it.copy(signingIn = false, signInError = errorText(error))
                    }
                    return@launch
                }
                loginHandle = handle
                openUrl(handle.url)

                val result = handle.poll()
                val account = result.getOrElse { error ->
                    _state.update {
                        it.copy(signingIn = false, signInError = errorText(error))
                    }
                    return@launch
                }

                _state.update {
                    it.copy(signingIn = false, signInError = null, account = account)
                }
                // A fresh login must produce a freshly bound engine: the core
                // reads the app password once at Start, so an engine still
                // running from before would keep using the old credentials.
                // Safe when nothing is running.
                NimboCore.stopEngine()
                refreshPermissions()
                if (_state.value.hasAllFiles) {
                    _state.update { it.copy(route = Route.HOME) }
                    startService()
                } else {
                    _state.update { it.copy(route = Route.PERMISSIONS) }
                }
            } catch (ce: CancellationException) {
                throw ce
            } catch (t: Throwable) {
                Log.w(TAG, "sign-in failed", t)
                _state.update { it.copy(signingIn = false, signInError = errorText(t)) }
            } finally {
                if (signInJob === self) {
                    loginHandle = null
                    signInJob = null
                }
            }
        }
    }

    fun cancelSignIn() {
        runCatching { loginHandle?.cancel() }
            .onFailure { Log.w(TAG, "cancelling login flow failed", it) }
        signInJob?.cancel()
        signInJob = null
        loginHandle = null
        _state.update { it.copy(signingIn = false, signInError = null) }
    }

    fun signOut() {
        viewModelScope.launch {
            _state.update { it.copy(busy = true) }
            // stopEngine waits for in-flight syncs to drain (bounded at ~30s by
            // the core), so anything that throws in here must still clear `busy`
            // or every Home control stays disabled until the app is restarted.
            try {
                val accountId = _state.value.account?.id
                    ?: NimboCore.accounts().getOrNull()?.firstOrNull()?.id
                stopService()
                NimboCore.stopEngine()
                var error: String? = null
                if (accountId != null && accountId.isNotEmpty()) {
                    NimboCore.logout(accountId).onFailure { error = errorText(it) }
                }
                // Whoever signs in next must not inherit this account's shade,
                // nor have its backlog announced to them as new.
                ServerNotifications.forget(getApplication())
                _state.value = UiState(route = Route.SIGN_IN)
                refreshPermissions()
                setMessage(error ?: "Signed out")
            } finally {
                _state.update { it.copy(busy = false) }
            }
        }
    }

    // ---------------------------------------------------------------- service

    fun startService() {
        val context = getApplication<Application>()
        runCatching {
            SyncService.start(context)
            SyncWorker.enqueuePeriodic(context)
        }.onFailure {
            Log.w(TAG, "could not start sync service", it)
            setMessage("Could not start the sync service: ${errorText(it)}")
        }
    }

    fun stopService() {
        val context = getApplication<Application>()
        runCatching {
            SyncService.stop(context)
            SyncWorker.cancel(context)
        }.onFailure { Log.w(TAG, "could not stop sync service", it) }
    }

    // ---------------------------------------------------------------- sync control

    fun syncNow() {
        viewModelScope.launch {
            _state.update { it.copy(busy = true) }
            try {
                if (!ensureEngine()) return@launch
                NimboCore.syncNow().onFailure { setMessage(errorText(it)) }
            } finally {
                _state.update { it.copy(busy = false) }
            }
        }
    }

    fun togglePause() {
        viewModelScope.launch {
            val target = !_state.value.paused
            NimboCore.setPaused(target).onFailure { setMessage(errorText(it)) }
        }
    }

    // ---------------------------------------------------------------- navigation

    fun navigate(route: Route) {
        _state.update { it.copy(route = route) }
        when (route) {
            Route.DIAGNOSTICS -> loadDiagnostics()
            Route.HOME -> loadQuota()
            else -> Unit
        }
    }

    // ---------------------------------------------------------------- file browser

    fun selectTab(tab: Tab) {
        _state.update { it.copy(tab = tab) }
        when (tab) {
            // Re-list on entry so the sync-state column reflects what the engine
            // has done since the tab was last looked at.
            Tab.FILES -> browseFiles(_state.value.filesPath)
            Tab.APPS -> loadApps()
            Tab.SYNC -> loadQuota()
        }
    }

    /** Lists [path] ("" is the account root) into the Files tab. */
    fun browseFiles(path: String) {
        viewModelScope.launch {
            _state.update { it.copy(filesLoading = true, filesPath = path, filesError = null) }
            try {
                if (!ensureEngine()) {
                    _state.update { it.copy(filesError = "Not connected") }
                    return@launch
                }
                FilesRepository.list(path)
                    .onSuccess { rows -> _state.update { it.copy(files = rows, filesError = null) } }
                    .onFailure { error ->
                        // Inline, not a snackbar: an empty list with no explanation
                        // is the single most confusing thing a browser can do.
                        _state.update { it.copy(filesError = errorText(error)) }
                    }
            } finally {
                _state.update { it.copy(filesLoading = false) }
            }
        }
    }

    /** Pops one segment off the browsed path; no-op at the account root. */
    fun filesUp() {
        val current = _state.value.filesPath.trim().trim('/')
        if (current.isEmpty()) return
        browseFiles(if (current.contains('/')) current.substringBeforeLast('/') else "")
    }

    fun refreshFiles() = browseFiles(_state.value.filesPath)

    /**
     * Opens a file: instantly when it is already synced, otherwise after
     * downloading it. [open] is supplied by the activity.
     *
     * The download carries no deadline, so `busy` stays set for as long as it
     * takes and the user can see the app is working rather than wedged.
     */
    fun openFile(row: FileRow, open: (Uri, String) -> Unit) {
        if (row.isDir) return
        viewModelScope.launch {
            _state.update { it.copy(busy = true) }
            try {
                if (!row.isOffline && !ensureEngine()) return@launch
                if (!row.isOffline) setMessage("Downloading ${row.name}…")
                FileOpener.openable(getApplication(), row)
                    .onSuccess { file -> open(file.uri, file.mimeType) }
                    .onFailure { error ->
                        setMessage("Couldn't open ${row.name}: ${errorText(error)}")
                    }
            } finally {
                _state.update { it.copy(busy = false) }
            }
        }
    }

    // ---------------------------------------------------------------- trash

    // ---------------------------------------------------------------- search

    fun openSearch() {
        _state.update {
            it.copy(
                route = Route.SEARCH,
                searchTerm = "",
                searchResults = emptyList(),
                searchError = null,
                searchRan = false,
            )
        }
    }

    fun onSearchTermChange(term: String) {
        _state.update { it.copy(searchTerm = term) }
    }

    /**
     * Runs the search. Deliberately on submit rather than on every keystroke:
     * each one is a WebDAV SEARCH across the whole account, and firing that per
     * character would hammer the server to show results the user is still
     * halfway through describing.
     */
    fun runSearch() {
        val term = _state.value.searchTerm.trim()
        if (term.isEmpty()) {
            _state.update { it.copy(searchResults = emptyList(), searchRan = false, searchError = null) }
            return
        }
        viewModelScope.launch {
            _state.update { it.copy(searchLoading = true, searchError = null) }
            try {
                if (!ensureEngine()) {
                    _state.update { it.copy(searchError = "Not connected", searchRan = true) }
                    return@launch
                }
                FilesRepository.search(term)
                    .onSuccess { rows ->
                        _state.update {
                            it.copy(
                                searchResults = rows,
                                searchError = null,
                                searchRan = true,
                                // Exactly the limit means the server may well have
                                // had more; presenting it as the full count would
                                // be a guess dressed up as a total.
                                searchTruncated = rows.size >= FilesRepository.SEARCH_LIMIT,
                            )
                        }
                    }
                    .onFailure { error ->
                        _state.update { it.copy(searchError = errorText(error), searchRan = true) }
                    }
            } finally {
                _state.update { it.copy(searchLoading = false) }
            }
        }
    }

    /** Opens a hit: a folder in the browser, a file however it opens elsewhere. */
    fun openSearchHit(row: FileRow, open: (Uri, String) -> Unit) {
        if (row.isDir) {
            _state.update { it.copy(route = Route.HOME, tab = Tab.FILES) }
            browseFiles(row.remotePath)
        } else {
            openFile(row, open)
        }
    }

    // ---------------------------------------------------------------- notifications

    /**
     * Opens the notifications screen. [focusId] is set when the user arrived by
     * tapping one in the shade: that specific notification is scrolled to and
     * marked, so they land on the thing they tapped rather than on a list they
     * then have to search.
     */
    fun openNotifications(focusId: Int? = null) {
        _state.update { it.copy(route = Route.NOTIFICATIONS, focusedNotificationId = focusId) }
        loadNotifications()
    }

    /** Drops the highlight once it has been seen, so it does not persist. */
    fun clearNotificationFocus() {
        _state.update { it.copy(focusedNotificationId = null) }
    }

    fun loadNotifications() {
        viewModelScope.launch {
            _state.update { it.copy(notificationsLoading = true, notificationsError = null) }
            try {
                if (!ensureEngine()) {
                    _state.update { it.copy(notificationsError = "Not connected") }
                    return@launch
                }
                // Ask the server rather than trusting the engine's cache: the
                // whole point of opening this screen is to see what is there now.
                NimboCore.refreshNotifications()
                NimboCore.notifications()
                    .onSuccess { list ->
                        _state.update {
                            // Trust the list we just fetched over a stale badge:
                            // a "3" beside an empty screen is worse than no badge.
                            it.copy(
                                notifications = list,
                                notificationCount = list.size,
                                notificationsError = null,
                            )
                        }
                    }
                    .onFailure { e -> _state.update { it.copy(notificationsError = errorText(e)) } }
            } finally {
                _state.update { it.copy(notificationsLoading = false) }
            }
        }
    }

    fun dismissNotification(item: NcNotification) {
        notificationAction { NimboCore.dismissNotification(item.id) }
    }

    fun dismissAllNotifications() {
        notificationAction { NimboCore.dismissAllNotifications() }
    }

    /**
     * Runs one of the notification's own actions. The link and method are the
     * server's, passed straight back — this app does not decide what "Accept"
     * means for an app it has never heard of.
     */
    fun runNotificationAction(action: NcNotificationAction) {
        notificationAction { NimboCore.doNotificationAction(action) }
    }

    private fun notificationAction(block: suspend () -> Result<Unit>) {
        viewModelScope.launch {
            _state.update { it.copy(busy = true) }
            try {
                if (!ensureEngine()) return@launch
                block()
                    .onSuccess { loadNotifications() }
                    .onFailure { setMessage(errorText(it)) }
            } finally {
                _state.update { it.copy(busy = false) }
            }
        }
    }

    // ---------------------------------------------------------------- sharing one file

    /** Opens the share sheet for a row and loads whatever already exists on it. */
    fun openShareSheet(row: FileRow) {
        _state.update {
            it.copy(sharingRow = row, rowShares = emptyList(), rowSharesError = null, sheetError = null)
        }
        loadRowShares()
    }

    fun closeShareSheet() {
        _state.update {
            it.copy(sharingRow = null, rowShares = emptyList(), rowSharesError = null, sheetError = null)
        }
    }

    fun loadRowShares() {
        val row = _state.value.sharingRow ?: return
        viewModelScope.launch {
            _state.update { it.copy(rowSharesLoading = true, rowSharesError = null) }
            try {
                if (!ensureEngine()) {
                    _state.update { it.copy(rowSharesError = "Not connected") }
                    return@launch
                }
                NimboCore.sharesOn(row.remotePath)
                    .onSuccess { list -> _state.update { it.copy(rowShares = list, rowSharesError = null) } }
                    .onFailure { e -> _state.update { it.copy(rowSharesError = errorText(e)) } }
            } finally {
                _state.update { it.copy(rowSharesLoading = false) }
            }
        }
    }

    /**
     * Creates a public link. The password is optional here but not always on the
     * server: an instance that requires one refuses the whole request, and that
     * refusal is shown rather than retried without protection.
     */
    fun createPublicLink(password: String, expiration: String) {
        val row = _state.value.sharingRow ?: return
        shareAction(
            success = { share ->
                if (share.url.isNotBlank()) "Link created for ${row.name}"
                else "Shared ${row.name}"
            },
        ) { NimboCore.createPublicLink(row.remotePath, password.trim(), expiration.trim()) }
    }

    fun createUserShare(user: String) {
        val row = _state.value.sharingRow ?: return
        shareAction(success = { "Shared ${row.name} with ${user.trim()}" }) {
            NimboCore.createUserShare(row.remotePath, user.trim())
        }
    }

    /**
     * Revokes a share. The wording says "access" deliberately — nothing is
     * deleted, and a user who thinks otherwise would hesitate over a safe action.
     */
    fun revokeShare(share: Share) {
        viewModelScope.launch {
            _state.update { it.copy(busy = true) }
            try {
                if (!ensureEngine()) return@launch
                NimboCore.deleteShare(share.id)
                    .onSuccess {
                        _state.update { it.copy(sheetError = null) }
                        setMessage("Access removed")
                        loadRowShares()
                    }
                    .onFailure { e -> _state.update { it.copy(sheetError = errorText(e)) } }
            } finally {
                _state.update { it.copy(busy = false) }
            }
        }
    }

    private fun shareAction(success: (Share) -> String, block: suspend () -> Result<Share>) {
        viewModelScope.launch {
            _state.update { it.copy(busy = true) }
            try {
                if (!ensureEngine()) return@launch
                block()
                    .onSuccess { share ->
                        _state.update { it.copy(sheetError = null) }
                        setMessage(success(share))
                        loadRowShares()
                    }
                    .onFailure { e -> _state.update { it.copy(sheetError = errorText(e)) } }
            } finally {
                _state.update { it.copy(busy = false) }
            }
        }
    }

    // ---------------------------------------------------------------- versions

    /**
     * Opens the version history for a file. Kept as a sheet over the browser
     * rather than a screen: it is about one row, and losing your place in a
     * long folder to read three dates would be a poor trade.
     */
    fun openVersions(row: FileRow) {
        if (row.isDir) return
        _state.update {
            it.copy(versionsFor = row, versions = emptyList(), versionsError = null, sheetError = null)
        }
        loadVersions()
    }

    fun closeVersions() {
        _state.update { it.copy(versionsFor = null, versions = emptyList(), versionsError = null, sheetError = null) }
    }

    fun loadVersions() {
        val row = _state.value.versionsFor ?: return
        if (row.fileId.isBlank()) {
            // No oc:fileid means the server never identified the file, and the
            // versions endpoint is addressed by nothing else.
            _state.update { it.copy(versionsError = "This file has no server id, so its history can't be looked up") }
            return
        }
        viewModelScope.launch {
            _state.update { it.copy(versionsLoading = true, versionsError = null) }
            try {
                if (!ensureEngine()) {
                    _state.update { it.copy(versionsError = "Not connected") }
                    return@launch
                }
                NimboCore.versions(row.fileId)
                    .onSuccess { list ->
                        _state.update { it.copy(versions = list.newestFirst(), versionsError = null) }
                    }
                    .onFailure { error -> _state.update { it.copy(versionsError = errorText(error)) } }
            } finally {
                _state.update { it.copy(versionsLoading = false) }
            }
        }
    }

    /**
     * Puts a previous revision back. The file's present contents become a
     * version in turn, which the message says — this is not a one-way door
     * while the server keeps versions.
     */
    fun restoreVersion(version: FileVersion) {
        val row = _state.value.versionsFor ?: return
        viewModelScope.launch {
            _state.update { it.copy(busy = true) }
            try {
                if (!ensureEngine()) return@launch
                NimboCore.restoreVersion(version.href)
                    .onSuccess {
                        setMessage("Restored ${row.name} to its ${version.whenLabel} version")
                        closeVersions()
                        refreshFiles()
                    }
                    // The sheet stays open on failure, so the snackbar would be
                    // hidden behind it.
                    .onFailure { e -> _state.update { it.copy(sheetError = errorText(e)) } }
            } finally {
                _state.update { it.copy(busy = false) }
            }
        }
    }

    // ---------------------------------------------------------------- shares

    fun openShares() {
        _state.update { it.copy(route = Route.SHARED) }
        loadShares()
    }

    fun loadShares() {
        viewModelScope.launch {
            _state.update { it.copy(sharesLoading = true, sharesError = null) }
            try {
                if (!ensureEngine()) {
                    _state.update { it.copy(sharesError = "Not connected") }
                    return@launch
                }
                NimboCore.shares()
                    .onSuccess { payload -> _state.update { it.copy(shares = payload, sharesError = null) } }
                    .onFailure { error -> _state.update { it.copy(sharesError = errorText(error)) } }
            } finally {
                _state.update { it.copy(sharesLoading = false) }
            }
        }
    }

    /**
     * Opens a share in the file browser.
     *
     * Only ever called for shares the user created: a RECEIVED share's path is
     * a path in the owner's account and need not exist in this one, so the
     * screen does not offer it.
     */
    fun openSharedPath(share: Share) {
        val path = share.path.trim().trim('/')
        _state.update { it.copy(route = Route.HOME, tab = Tab.FILES) }
        browseFiles(if (share.isFolder) path else path.substringBeforeLast('/', ""))
    }

    // ---------------------------------------------------------------- favourites

    fun openFavorites() {
        _state.update { it.copy(route = Route.FAVORITES) }
        loadFavorites()
    }

    fun loadFavorites() {
        viewModelScope.launch {
            _state.update { it.copy(favoritesLoading = true, favoritesError = null) }
            try {
                if (!ensureEngine()) {
                    _state.update { it.copy(favoritesError = "Not connected") }
                    return@launch
                }
                FilesRepository.favorites()
                    .onSuccess { rows -> _state.update { it.copy(favorites = rows, favoritesError = null) } }
                    .onFailure { error -> _state.update { it.copy(favoritesError = errorText(error)) } }
            } finally {
                _state.update { it.copy(favoritesLoading = false) }
            }
        }
    }

    /**
     * Opens a favourite. A starred folder lands the user in the Files browser at
     * that folder — favourites are shortcuts into the tree, not a place to browse
     * from — while a file opens as it would anywhere else.
     */
    fun openFavorite(row: FileRow, open: (Uri, String) -> Unit) {
        if (row.isDir) {
            _state.update { it.copy(route = Route.HOME, tab = Tab.FILES) }
            browseFiles(row.remotePath)
        } else {
            openFile(row, open)
        }
    }

    /**
     * Stars or unstars a row, then re-reads whichever list the user is looking
     * at. The favourites screen must refresh even on an unstar that empties it —
     * leaving the row on screen would suggest the star did not take.
     */
    fun toggleFavorite(row: FileRow) {
        viewModelScope.launch {
            _state.update { it.copy(busy = true) }
            try {
                if (!ensureEngine()) return@launch
                val starring = !row.isFavorite
                NimboCore.setFavorite(row.remotePath, starring)
                    .onSuccess {
                        setMessage(
                            if (starring) "Added ${row.name} to favourites"
                            else "Removed ${row.name} from favourites"
                        )
                        if (_state.value.route == Route.FAVORITES) loadFavorites() else refreshFiles()
                    }
                    .onFailure { setMessage(errorText(it)) }
            } finally {
                _state.update { it.copy(busy = false) }
            }
        }
    }

    fun openTrash() {
        _state.update { it.copy(route = Route.TRASH) }
        loadTrash()
    }

    fun loadTrash() {
        viewModelScope.launch {
            _state.update { it.copy(trashLoading = true, trashError = null) }
            try {
                if (!ensureEngine()) {
                    _state.update { it.copy(trashError = "Not connected") }
                    return@launch
                }
                NimboCore.trash()
                    .onSuccess { items -> _state.update { it.copy(trash = items, trashError = null) } }
                    .onFailure { error -> _state.update { it.copy(trashError = errorText(error)) } }
            } finally {
                _state.update { it.copy(trashLoading = false) }
            }
        }
    }

    /**
     * Restores an item. It comes back on the SERVER; anything inside a synced
     * pair reappears locally on the next pass, which the message says so the
     * user is not left watching a folder that has not changed yet.
     */
    fun restoreTrashItem(item: TrashItem) {
        trashAction("Restored “${item.name}” — it returns to this device on the next sync") {
            NimboCore.restoreTrash(item.href)
        }
    }

    fun deleteTrashItemForever(item: TrashItem) {
        trashAction("Deleted “${item.name}” permanently") {
            NimboCore.deleteTrashItem(item.href)
        }
    }

    private fun trashAction(success: String, block: suspend () -> Result<Unit>) {
        viewModelScope.launch {
            _state.update { it.copy(busy = true) }
            try {
                if (!ensureEngine()) return@launch
                block()
                    .onSuccess { setMessage(success); loadTrash() }
                    .onFailure { setMessage(errorText(it)) }
            } finally {
                _state.update { it.copy(busy = false) }
            }
        }
    }

    // ---------------------------------------------------------------- damage guard

    /**
     * Re-reads the paused set and the per-folder health. Quiet: empty lists are
     * the normal case.
     */
    fun refreshFrozen() {
        viewModelScope.launch {
            if (!NimboCore.isRunning()) return@launch
            NimboCore.frozenFolders()
                .onSuccess { list -> _state.update { it.copy(frozen = list) } }
                .onFailure { Log.w(TAG, "could not read the paused folders", it) }
            NimboCore.failingFolders()
                .onSuccess { list -> _state.update { it.copy(failing = list) } }
                .onFailure { Log.w(TAG, "could not read folder health", it) }
        }
    }

    /**
     * Resumes a folder the guard paused. The engine refuses if it is not
     * actually paused, so the list is re-read either way rather than trusting
     * what was on screen.
     */
    fun resumeFrozen(localDir: String) {
        viewModelScope.launch {
            _state.update { it.copy(busy = true) }
            try {
                if (!ensureEngine()) return@launch
                NimboCore.clearFreeze(localDir)
                    .onSuccess { setMessage("Resumed syncing") }
                    .onFailure { setMessage(errorText(it)) }
                refreshFrozen()
            } finally {
                _state.update { it.copy(busy = false) }
            }
        }
    }

    // ---------------------------------------------------------------- file actions

    /** Creates a folder in the folder currently being browsed. */
    fun createFolder(name: String) = fileAction("Created “$name”") {
        NimboCore.mkdirRemote(joinRemote(_state.value.filesPath, name))
    }

    fun renameEntry(row: FileRow, newName: String) {
        val target = renameTargetRemote(row.remotePath, newName)
        if (target == null) {
            setMessage(remoteNameProblem(newName) ?: "That name can't be used")
            return
        }
        if (target == row.remotePath) return
        fileAction("Renamed to “${newName.trim()}”") { NimboCore.moveRemote(row.remotePath, target) }
    }

    /**
     * Deletes from the server. Anything inside a synced pair also loses its local
     * copy on the next pass — the confirm dialog says so before we get here.
     */
    fun deleteEntry(row: FileRow) = fileAction("Deleted “${row.name}”") {
        NimboCore.deleteRemote(row.remotePath)
    }

    /** Uploads a document chosen with the system picker into the current folder. */
    fun uploadFrom(uri: Uri) {
        viewModelScope.launch {
            _state.update { it.copy(busy = true) }
            var staged: StagedUpload? = null
            try {
                if (!ensureEngine()) return@launch
                staged = Uploads.stage(getApplication(), uri).getOrElse { error ->
                    setMessage("Couldn't read that file: ${errorText(error)}")
                    return@launch
                }
                setMessage("Uploading ${staged.displayName}…")
                val remote = joinRemote(_state.value.filesPath, staged.displayName)
                NimboCore.uploadFile(staged.file.absolutePath, remote)
                    .onSuccess {
                        setMessage("Uploaded ${staged?.displayName}")
                        refreshFiles()
                    }
                    .onFailure { setMessage("Upload failed: ${errorText(it)}") }
            } finally {
                // The staged copy exists only for the upload; it must not sit in
                // the cache doubling the space the file already takes.
                staged?.discard()
                _state.update { it.copy(busy = false) }
            }
        }
    }

    /** Shared shape for the mutations: busy latch, refresh on success, message either way. */
    private fun fileAction(success: String, block: suspend () -> Result<Unit>) {
        viewModelScope.launch {
            _state.update { it.copy(busy = true) }
            try {
                if (!ensureEngine()) return@launch
                block()
                    .onSuccess {
                        setMessage(success)
                        refreshFiles()
                    }
                    .onFailure { setMessage(errorText(it)) }
            } finally {
                _state.update { it.copy(busy = false) }
            }
        }
    }

    fun openFolders() {
        _state.update { it.copy(route = Route.FOLDERS) }
        browse("")
    }

    fun browse(path: String) {
        viewModelScope.launch {
            _state.update { it.copy(browseLoading = true, browsePath = path) }
            try {
                if (!ensureEngine()) return@launch
                NimboCore.browse(path)
                    .onSuccess { entries ->
                        // A depth-1 PROPFIND lists the directory itself alongside
                        // its children. Drop it, or the picker shows a nameless
                        // first row that navigates back to where you already are.
                        val here = path.trim().trim('/')
                        val children = entries
                            .filter { it.path.trim().trim('/') != here }
                            // Same order as the Files tab: the server returns its
                            // own order, and two different orders for the same
                            // folders in one app reads as a bug.
                            .sortedWith(
                                compareByDescending<BrowseEntry> { it.isDir }
                                    .thenBy(String.CASE_INSENSITIVE_ORDER) { it.name }
                            )
                        _state.update { it.copy(browseEntries = children) }
                    }
                    .onFailure { setMessage(errorText(it)) }
            } finally {
                _state.update { it.copy(browseLoading = false) }
            }
        }
    }

    /** Pops one segment off the current browse path; no-op at the root. */
    fun browseUp() {
        val current = _state.value.browsePath.trim().trim('/')
        if (current.isEmpty()) return
        val parent = if (current.contains('/')) current.substringBeforeLast('/') else ""
        browse(parent)
    }

    // ---------------------------------------------------------------- folders

    /**
     * Step 1 of adding a folder: the user picked a remote folder. Work out where
     * it would land by default and show the confirm step, where they can change
     * the destination before anything is created.
     */
    fun selectRemoteFolder(remoteRoot: String) {
        viewModelScope.launch {
            _state.update { it.copy(busy = true) }
            try {
                if (!ensureEngine()) return@launch
                val base = resolveBaseDir()
                _state.update {
                    it.copy(
                        route = Route.ADD_FOLDER,
                        baseDir = base,
                        pendingRemoteRoot = remoteRoot,
                        pendingLocalDir = LocalFs.defaultLocalFor(base, remoteRoot),
                    )
                }
            } finally {
                _state.update { it.copy(busy = false) }
            }
        }
    }

    /** Opens the local browser at the pending destination's parent. */
    fun openLocalPicker() {
        val current = _state.value
        val start = LocalFs.parentOf(current.pendingLocalDir)
            ?: current.baseDir.ifBlank { LocalFs.externalRoot() }
        _state.update { it.copy(route = Route.LOCAL_PICKER) }
        browseLocal(start)
    }

    fun browseLocal(path: String) {
        viewModelScope.launch {
            val dirs = withContext(Dispatchers.IO) { LocalFs.listDirs(path) }
            _state.update {
                it.copy(
                    localPath = path,
                    localDirs = dirs,
                    localCanGoUp = LocalFs.parentOf(path) != null,
                )
            }
        }
    }

    fun browseLocalUp() {
        val parent = LocalFs.parentOf(_state.value.localPath) ?: return
        browseLocal(parent)
    }

    fun createLocalFolder(name: String) {
        viewModelScope.launch {
            val parent = _state.value.localPath
            withContext(Dispatchers.IO) { LocalFs.createDir(parent, name) }
                .onSuccess { created -> browseLocal(created) }
                .onFailure { setMessage(errorText(it)) }
        }
    }

    /**
     * The user chose [dir] as the destination — exactly that directory, not a
     * subfolder of it. The picker's action reads "Sync here" over the path it is
     * showing, and it can create a folder first, so appending the remote name
     * here would silently double-nest (a new "Photos" folder would become
     * Photos/Photos). The confirm step shows the final path before anything is
     * created.
     */
    fun useLocalDir(dir: String) {
        _state.update {
            it.copy(route = Route.ADD_FOLDER, pendingLocalDir = dir)
        }
    }

    /** Step 2: create the pair at the confirmed destination. */
    fun confirmAddFolder() {
        viewModelScope.launch {
            val snapshot = _state.value
            val remote = snapshot.pendingRemoteRoot
            val local = snapshot.pendingLocalDir
            // A blank remote is legitimate — it is the account root ("sync
            // everything"). Only a missing local destination is a real problem,
            // and it must say so rather than silently doing nothing.
            if (local.isBlank()) {
                setMessage("Choose where this folder should live on your phone")
                return@launch
            }
            _state.update { it.copy(busy = true) }
            try {
                if (!ensureEngine()) return@launch
                // When the destination is the default one AND the core really is
                // pointing at that base dir, let addSyncFolder do it so the core
                // keeps ownership of its own layout. Any other path — including a
                // base dir that failed to persist — becomes an explicit pair,
                // which the core does not guard because we chose the path.
                val default = LocalFs.defaultLocalFor(snapshot.baseDir, remote)
                val baseDirLive = snapshot.baseDir.isNotBlank() &&
                    NimboCore.baseDir().getOrNull() == snapshot.baseDir
                // Whole-account sync always goes through addSyncPair: addSyncFolder
                // derives the local folder from the remote's name, and the root
                // has none.
                val result = if (remote.isNotBlank() && baseDirLive && local == default) {
                    NimboCore.addSyncFolder(remote)
                } else {
                    NimboCore.addSyncPair(local, remote)
                }
                result
                    .onSuccess {
                        _state.update {
                            it.copy(
                                route = Route.HOME,
                                pendingRemoteRoot = "",
                                pendingLocalDir = "",
                            )
                        }
                        setMessage("Syncing ${displayName(remote)}")
                        loadQuotaQuietly()
                    }
                    .onFailure { setMessage(errorText(it)) }
            } finally {
                _state.update { it.copy(busy = false) }
            }
        }
    }

    /**
     * The core's sync root, remembered once resolved. If the core will not
     * persist it (see NimboCore.setBaseDirWithRetry) we still return the
     * conventional path so the UI can offer a sensible default — adding the
     * folder then goes through addSyncPair, which needs no base dir at all.
     */
    private suspend fun resolveBaseDir(): String {
        val existing = _state.value.baseDir
        if (existing.isNotBlank()) return existing
        val dir = NimboCore.ensureBaseDir().getOrElse { error ->
            Log.w(TAG, "ensureBaseDir failed; using the conventional path", error)
            NimboCore.defaultBaseDir()
        }
        _state.update { it.copy(baseDir = dir) }
        return dir
    }

    fun removeFolder(remoteRoot: String, deleteLocal: Boolean) {
        viewModelScope.launch {
            _state.update { it.copy(busy = true) }
            try {
                if (!ensureEngine()) return@launch
                NimboCore.removeSyncFolder(remoteRoot, deleteLocal)
                    .onSuccess { setMessage("Removed ${displayName(remoteRoot)}") }
                    .onFailure { setMessage(errorText(it)) }
            } finally {
                _state.update { it.copy(busy = false) }
            }
        }
    }

    // ---------------------------------------------------------------- info screens

    fun loadApps() {
        viewModelScope.launch {
            if (!ensureEngine()) return@launch
            // The navigation API returns each app's href relative to the server
            // root, so without the server URL there is nothing openable to hand
            // the browser — fetch it before the tiles can be tapped.
            if (_state.value.serverUrl.isBlank()) {
                NimboCore.serverUrl()
                    .onSuccess { url -> _state.update { it.copy(serverUrl = url) } }
                    .onFailure { Log.w(TAG, "could not read the server URL", it) }
            }
            NimboCore.apps()
                .onSuccess { apps -> _state.update { it.copy(apps = apps) } }
                .onFailure { setMessage(errorText(it)) }
        }
    }

    fun loadDiagnostics() {
        viewModelScope.launch {
            if (!ensureEngine()) return@launch
            NimboCore.diagnostics()
                .onSuccess { d -> _state.update { it.copy(diagnostics = d) } }
                .onFailure { setMessage(errorText(it)) }
        }
    }

    fun loadQuota() {
        viewModelScope.launch { loadQuotaQuietly() }
    }

    /** Quota is ancillary — a failure here must not spam the snackbar. */
    private suspend fun loadQuotaQuietly() {
        NimboCore.quota()
            .onSuccess { q -> _state.update { it.copy(quota = q) } }
            .onFailure { Log.d(TAG, "quota unavailable: ${errorText(it)}") }
    }

    // ---------------------------------------------------------------- messages

    fun consumeMessage() {
        _state.update { it.copy(message = null) }
    }

    private fun setMessage(text: String) {
        _state.update { it.copy(message = text) }
    }

    // ---------------------------------------------------------------- helpers

    /**
     * Most engine calls need a running engine. The service normally owns it, but
     * the user can reach a screen before the service has booted it, so bring it
     * up here too — startEngine() is mutex-guarded and returns success when the
     * engine is already running.
     */
    /**
     * Makes sure the engine is up before an action that needs it.
     *
     * Bounded: Start blocks on the server's capabilities fetch, and on a captive
     * portal or a half-dead server that wait is long. Callers hold the `busy`
     * latch across this, so without a ceiling the Home controls would sit
     * disabled for minutes with no explanation.
     */
    private suspend fun ensureEngine(): Boolean {
        if (withContext(Dispatchers.IO) { NimboCore.isRunning() }) return true
        startService()
        val result = withTimeoutOrNull(ENGINE_START_TIMEOUT_MS) { NimboCore.startEngine() }
        if (result == null) {
            setMessage("Couldn't reach your server — check the connection and try again")
            return false
        }
        result.onFailure { setMessage(errorText(it)) }
        return result.isSuccess
    }

    private fun errorText(t: Throwable?): String {
        if (t == null) return "Unknown error"
        val message = t.message
        return if (message.isNullOrBlank()) t.javaClass.simpleName else message
    }

    private fun displayName(remoteRoot: String): String {
        val trimmed = remoteRoot.trim().trim('/')
        if (trimmed.isEmpty()) return "your files"
        return trimmed.substringAfterLast('/')
    }
}
