/*
 * NimboNav.kt — the app's whole navigation layer: a `when` over UiState.route.
 *
 * No androidx.navigation by design (see UiState.kt). This composable also owns the
 * snackbar plumbing (state.message -> show -> vm.consumeMessage()) and system-back
 * handling for the secondary screens. Anything that needs an Activity — Custom
 * Tabs, the notification-permission dialog, settings intents — goes through the
 * NimboHost provided by MainActivity, so this file stays free of Activity lookups.
 */
package dev.otherworld.nimbo.ui

import androidx.activity.compose.BackHandler
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import dev.otherworld.nimbo.LocalNimboHost
import dev.otherworld.nimbo.core.absoluteUrl
import dev.otherworld.nimbo.ui.screens.AddFolderScreen
import dev.otherworld.nimbo.ui.screens.AppsScreen
import dev.otherworld.nimbo.ui.screens.DiagnosticsScreen
import dev.otherworld.nimbo.ui.screens.FavoritesScreen
import dev.otherworld.nimbo.ui.screens.FilesScreen
import dev.otherworld.nimbo.ui.screens.FolderPickerScreen
import dev.otherworld.nimbo.ui.screens.HomeScreen
import dev.otherworld.nimbo.ui.screens.LocalFolderPickerScreen
import dev.otherworld.nimbo.ui.screens.NotificationsScreen
import dev.otherworld.nimbo.ui.screens.PermissionsScreen
import dev.otherworld.nimbo.ui.screens.SearchScreen
import dev.otherworld.nimbo.ui.screens.SettingsScreen
import dev.otherworld.nimbo.ui.screens.ShareSheet
import dev.otherworld.nimbo.ui.screens.SharedScreen
import dev.otherworld.nimbo.ui.screens.VersionsSheet
import dev.otherworld.nimbo.ui.screens.SignInScreen
import dev.otherworld.nimbo.ui.screens.TrashScreen

@Composable
fun NimboNav(vm: NimboViewModel) {
    val state by vm.state.collectAsStateWithLifecycle()
    val host = LocalNimboHost.current
    val snackbarHostState = remember { SnackbarHostState() }

    val message = state.message
    LaunchedEffect(message) {
        if (message == null) return@LaunchedEffect
        if (message.isNotBlank()) {
            // Suspends until the snackbar is dismissed or a newer message replaces
            // it (this effect restarts and cancels the previous show).
            snackbarHostState.showSnackbar(message)
        }
        vm.consumeMessage()
    }

    // Back unwinds the add-a-folder flow one step at a time
    // (local picker -> confirm -> remote picker -> home) and otherwise returns
    // to Home; on Home itself it falls through to the system (leaves the app).
    val inFileSubfolder = state.route == Route.HOME &&
        state.tab == Tab.FILES &&
        state.filesPath.trim().trim('/').isNotEmpty()

    BackHandler(
        enabled = state.route == Route.FOLDERS ||
            state.route == Route.ADD_FOLDER ||
            state.route == Route.LOCAL_PICKER ||
            state.route == Route.DIAGNOSTICS ||
            state.route == Route.TRASH ||
            state.route == Route.FAVORITES ||
            state.route == Route.SHARED ||
            state.route == Route.SEARCH ||
            state.route == Route.NOTIFICATIONS ||
            state.route == Route.SETTINGS ||
            inFileSubfolder
    ) {
        when {
            state.route == Route.LOCAL_PICKER -> vm.navigate(Route.ADD_FOLDER)
            state.route == Route.ADD_FOLDER -> vm.navigate(Route.FOLDERS)
            // Inside the browser, back walks up the tree rather than leaving.
            inFileSubfolder -> vm.filesUp()
            else -> vm.navigate(Route.HOME)
        }
    }

    Scaffold(
        // The individual screens own their window insets (they carry their own
        // top bars), so this outer Scaffold consumes none of them.
        contentWindowInsets = WindowInsets(0, 0, 0, 0),
        snackbarHost = {
            SnackbarHost(
                hostState = snackbarHostState,
                modifier = Modifier
                    .navigationBarsPadding()
                    .imePadding(),
            )
        },
    ) { innerPadding ->
        Box(
            modifier = Modifier
                .fillMaxSize()
                .padding(innerPadding)
        ) {
            when (state.route) {
                Route.LOADING -> Box(
                    modifier = Modifier.fillMaxSize(),
                    contentAlignment = Alignment.Center,
                ) {
                    CircularProgressIndicator()
                }

                Route.SIGN_IN -> SignInScreen(
                    serverUrl = state.serverUrlInput,
                    signingIn = state.signingIn,
                    error = state.signInError,
                    onServerUrlChange = { value -> vm.onServerUrlChange(value) },
                    onSignIn = { vm.signIn { url -> host.openUrl(url) } },
                    onCancel = { vm.cancelSignIn() },
                )

                Route.PERMISSIONS -> PermissionsScreen(
                    hasAllFiles = state.hasAllFiles,
                    hasNotifications = state.hasNotifications,
                    ignoringBattery = state.ignoringBattery,
                    onGrantAllFiles = { host.openAllFilesSettings() },
                    onGrantNotifications = { host.requestNotificationPermission() },
                    onGrantBattery = { host.openBatteryExemptionSettings() },
                    onContinue = { vm.onPermissionsContinue() },
                )

                Route.HOME -> TabHost(
                    tab = state.tab,
                    onSelectTab = { tab -> vm.selectTab(tab) },
                    syncing = state.progress?.active == true,
                ) {
                    when (state.tab) {
                        Tab.FILES -> FilesScreen(
                            path = state.filesPath,
                            rows = state.files,
                            loading = state.filesLoading,
                            error = state.filesError,
                            onOpenFolder = { p -> vm.browseFiles(p) },
                            onOpenFile = { row ->
                                vm.openFile(row) { uri, mime -> host.openFile(uri, mime) }
                            },
                            onUp = { vm.filesUp() },
                            onRefresh = { vm.refreshFiles() },
                            busy = state.busy,
                            onCreateFolder = { name -> vm.createFolder(name) },
                            onRename = { row, name -> vm.renameEntry(row, name) },
                            onDelete = { row -> vm.deleteEntry(row) },
                            onUpload = { host.pickFileToUpload() },
                            onToggleFavorite = { row -> vm.toggleFavorite(row) },
                            onOpenVersions = { row -> vm.openVersions(row) },
                            onShare = { row -> vm.openShareSheet(row) },
                            onOpenFavorites = { vm.openFavorites() },
                            onOpenShared = { vm.openShares() },
                            onOpenSearch = { vm.openSearch() },
                            onOpenNotifications = { vm.openNotifications() },
                            notificationCount = state.notificationCount,
                            onOpenTrash = { vm.openTrash() },
                        )

                        Tab.SYNC -> HomeScreen(
                            status = state.status,
                            progress = state.progress,
                            paused = state.paused,
                            running = state.running,
                            pairs = state.pairs,
                            frozen = state.frozen,
                            failing = state.failing,
                            quota = state.quota,
                            lastSynced = state.lastSynced,
                            lastSyncAt = state.lastSyncAt,
                            busy = state.busy,
                            onSyncNow = { vm.syncNow() },
                            onTogglePause = { vm.togglePause() },
                            onAddFolder = { vm.openFolders() },
                            onRemoveFolder = { remoteRoot -> vm.removeFolder(remoteRoot, false) },
                            onOpenApps = { vm.selectTab(Tab.APPS) },
                            onOpenSettings = { vm.openSettings() },
                            onOpenDiagnostics = { vm.navigate(Route.DIAGNOSTICS) },
                            onOpenNotifications = { vm.openNotifications() },
                            notificationCount = state.notificationCount,
                            onSignOut = { vm.signOut() },
                            onResumeFrozen = { dir -> vm.resumeFrozen(dir) },
                        )

                        Tab.APPS -> AppsScreen(
                            apps = state.apps,
                            // Hrefs arrive relative to the server root; resolve before opening.
                            onOpen = { app -> host.openUrl(absoluteUrl(state.serverUrl, app.href)) },
                            onBack = null, // a tab, not a pushed screen
                            onOpenNotifications = { vm.openNotifications() },
                            notificationCount = state.notificationCount,
                        )
                    }
                }

                Route.FOLDERS -> FolderPickerScreen(
                    path = state.browsePath,
                    entries = state.browseEntries,
                    loading = state.browseLoading,
                    onOpen = { path -> vm.browse(path) },
                    onUp = { vm.browseUp() },
                    onSelect = { path -> vm.selectRemoteFolder(path) },
                    onBack = { vm.navigate(Route.HOME) },
                )

                Route.ADD_FOLDER -> AddFolderScreen(
                    remoteRoot = state.pendingRemoteRoot,
                    localDir = state.pendingLocalDir,
                    busy = state.busy,
                    onChangeLocal = { vm.openLocalPicker() },
                    onConfirm = { vm.confirmAddFolder() },
                    onBack = { vm.navigate(Route.FOLDERS) },
                )

                Route.LOCAL_PICKER -> LocalFolderPickerScreen(
                    path = state.localPath,
                    dirs = state.localDirs,
                    canGoUp = state.localCanGoUp,
                    onOpen = { path -> vm.browseLocal(path) },
                    onUp = { vm.browseLocalUp() },
                    onCreateFolder = { name -> vm.createLocalFolder(name) },
                    onUse = { path -> vm.useLocalDir(path) },
                    onBack = { vm.navigate(Route.ADD_FOLDER) },
                )

                Route.NOTIFICATIONS -> NotificationsScreen(
                    items = state.notifications,
                    loading = state.notificationsLoading,
                    error = state.notificationsError,
                    busy = state.busy,
                    focusedId = state.focusedNotificationId,
                    onFocusShown = { vm.clearNotificationFocus() },
                    onRunAction = { action -> vm.runNotificationAction(action) },
                    // Notification links point into the web UI, so they open
                    // there rather than pretending the app can render them.
                    onOpenLink = { item ->
                        host.openUrl(absoluteUrl(state.serverUrl, item.link))
                    },
                    onDismiss = { item -> vm.dismissNotification(item) },
                    onDismissAll = { vm.dismissAllNotifications() },
                    onRefresh = { vm.loadNotifications() },
                    onBack = { vm.navigate(Route.HOME) },
                )

                Route.SEARCH -> SearchScreen(
                    term = state.searchTerm,
                    results = state.searchResults,
                    loading = state.searchLoading,
                    error = state.searchError,
                    hasSearched = state.searchRan,
                    truncated = state.searchTruncated,
                    onTermChange = { value -> vm.onSearchTermChange(value) },
                    onSubmit = { vm.runSearch() },
                    onOpen = { row ->
                        vm.openSearchHit(row) { uri, mime -> host.openFile(uri, mime) }
                    },
                    onBack = { vm.navigate(Route.HOME) },
                )

                Route.SHARED -> SharedScreen(
                    shares = state.shares,
                    loading = state.sharesLoading,
                    error = state.sharesError,
                    onOpenOwn = { share -> vm.openSharedPath(share) },
                    onCopyLink = { share -> host.copyToClipboard(share.url, "Link copied") },
                    onRefresh = { vm.loadShares() },
                    onBack = { vm.navigate(Route.HOME) },
                )

                Route.FAVORITES -> FavoritesScreen(
                    rows = state.favorites,
                    loading = state.favoritesLoading,
                    error = state.favoritesError,
                    busy = state.busy,
                    onOpen = { row ->
                        vm.openFavorite(row) { uri, mime -> host.openFile(uri, mime) }
                    },
                    onToggleFavorite = { row -> vm.toggleFavorite(row) },
                    onRefresh = { vm.loadFavorites() },
                    onBack = { vm.navigate(Route.HOME) },
                )

                Route.TRASH -> TrashScreen(
                    items = state.trash,
                    loading = state.trashLoading,
                    error = state.trashError,
                    busy = state.busy,
                    onRestore = { item -> vm.restoreTrashItem(item) },
                    onDeleteForever = { item -> vm.deleteTrashItemForever(item) },
                    onRefresh = { vm.loadTrash() },
                    onBack = { vm.navigate(Route.HOME) },
                )

                Route.SETTINGS -> SettingsScreen(
                    appearance = state.appearance,
                    serverAppearance = state.serverAppearance,
                    themeColor = state.themeColor,
                    onAppearanceChange = { pref -> vm.setAppearance(pref) },
                    onBack = { vm.navigate(Route.HOME) },
                )

                Route.DIAGNOSTICS -> DiagnosticsScreen(
                    diagnostics = state.diagnostics,
                    onRefresh = { vm.loadDiagnostics() },
                    onBack = { vm.navigate(Route.HOME) },
                )
            }

            // Sits above the route, not inside it: version history is about one
            // row, and reaching it should not cost the user their place.
            state.sharingRow?.let { row ->
                ShareSheet(
                    row = row,
                    shares = state.rowShares,
                    loading = state.rowSharesLoading,
                    error = state.rowSharesError,
                    actionError = state.sheetError,
                    busy = state.busy,
                    onCreateLink = { pw, exp -> vm.createPublicLink(pw, exp) },
                    onShareWithUser = { user -> vm.createUserShare(user) },
                    onRevoke = { share -> vm.revokeShare(share) },
                    onCopyLink = { share -> host.copyToClipboard(share.url, "Link copied") },
                    onDismiss = { vm.closeShareSheet() },
                )
            }

            state.versionsFor?.let { row ->
                VersionsSheet(
                    row = row,
                    versions = state.versions,
                    loading = state.versionsLoading,
                    error = state.versionsError,
                    actionError = state.sheetError,
                    busy = state.busy,
                    onRestore = { version -> vm.restoreVersion(version) },
                    onDismiss = { vm.closeVersions() },
                )
            }
        }
    }
}
