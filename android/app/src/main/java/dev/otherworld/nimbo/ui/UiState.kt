/*
 * UiState.kt — the single immutable snapshot the whole UI renders from, plus the
 * Route enum that stands in for a navigation library (we deliberately do not use
 * androidx.navigation: the app has a handful of screens and the engine owns all
 * real state, so a route field in the state is simpler and easier to reason about).
 */
package dev.otherworld.nimbo.ui

import dev.otherworld.nimbo.core.BrowseEntry
import dev.otherworld.nimbo.core.Diagnostics
import dev.otherworld.nimbo.core.FailingFolder
import dev.otherworld.nimbo.core.FileRow
import dev.otherworld.nimbo.core.FileVersion
import dev.otherworld.nimbo.core.FrozenFolder
import dev.otherworld.nimbo.core.NcAccount
import dev.otherworld.nimbo.core.NcApp
import dev.otherworld.nimbo.core.NcNotification
import dev.otherworld.nimbo.core.PairSyncedEvent
import dev.otherworld.nimbo.core.Quota
import dev.otherworld.nimbo.core.Share
import dev.otherworld.nimbo.core.SharesPayload
import dev.otherworld.nimbo.core.SyncPair
import dev.otherworld.nimbo.core.SyncProgress
import dev.otherworld.nimbo.core.TrashItem
import dev.otherworld.nimbo.platform.LocalDir
import dev.otherworld.nimbo.ui.theme.AppearancePreference

/** The three top-level tabs behind [Route.HOME]. */
enum class Tab { FILES, SYNC, APPS }

/** Every screen the app can show. [Route.HOME] hosts the tabs; the rest are full-screen. */
enum class Route {
    LOADING,
    SIGN_IN,
    PERMISSIONS,
    /** The bottom-nav host: Files / Sync / Apps. */
    HOME,
    FOLDERS,
    /** Confirm step: which remote folder, and where it lands on the phone. */
    ADD_FOLDER,
    /** Local filesystem browser for choosing that destination. */
    LOCAL_PICKER,
    /** Server notifications. */
    NOTIFICATIONS,
    /** App preferences. */
    SETTINGS,
    /** Starred files and folders. */
    FAVORITES,
    /** What the user shared out, and what was shared with them. */
    SHARED,
    /** Search across the whole account, by name. */
    SEARCH,
    /** Server trashbin: restore or delete permanently. */
    TRASH,
    DIAGNOSTICS,
}

data class UiState(
    val route: Route = Route.LOADING,
    val tab: Tab = Tab.FILES,

    // -- file browser -------------------------------------------------------
    /** Account-relative path being browsed; "" is the account root. */
    val filesPath: String = "",
    val files: List<FileRow> = emptyList(),
    val filesLoading: Boolean = false,
    val filesError: String? = null,

    val serverUrlInput: String = "",
    val signingIn: Boolean = false,
    val signInError: String? = null,
    val account: NcAccount? = null,
    /** Account server URL — app hrefs from the API are relative to it. */
    val serverUrl: String = "",
    /** The user's Nextcloud theme colour, e.g. "#0082c9"; blank until known. */
    val themeColor: String = "",
    /** What Nextcloud reports for appearance: dark / light / default. */
    val serverAppearance: String = "",
    val appearance: AppearancePreference = AppearancePreference.FOLLOW_NEXTCLOUD,
    val status: String = "",
    val progress: SyncProgress? = null,
    val paused: Boolean = false,
    val running: Boolean = false,
    val pairs: List<SyncPair> = emptyList(),
    /** Folders the damage guard has paused; empty is normal. */
    val frozen: List<FrozenFolder> = emptyList(),
    /** Folders whose last pass failed; blocks any "up to date" claim. */
    val failing: List<FailingFolder> = emptyList(),
    val quota: Quota? = null,
    val apps: List<NcApp> = emptyList(),
    val diagnostics: Diagnostics? = null,
    val searchTerm: String = "",
    val searchResults: List<FileRow> = emptyList(),
    val searchLoading: Boolean = false,
    val searchError: String? = null,
    /** True once a search has actually run, so "no matches" is only shown after one. */
    val searchRan: Boolean = false,
    /** The server had more hits than we asked for; the list is not the whole answer. */
    val searchTruncated: Boolean = false,
    /**
     * An error from an action taken INSIDE a bottom sheet. Snackbars render
     * behind a ModalBottomSheet, so a failure reported that way is invisible —
     * the sheet has to show its own.
     */
    val sheetError: String? = null,
    /** The row whose share sheet is open; null = closed. */
    val sharingRow: FileRow? = null,
    val rowShares: List<Share> = emptyList(),
    val rowSharesLoading: Boolean = false,
    val rowSharesError: String? = null,
    /** The file whose versions are open in the sheet; null = closed. */
    val versionsFor: FileRow? = null,
    val versions: List<FileVersion> = emptyList(),
    val versionsLoading: Boolean = false,
    val versionsError: String? = null,
    val notifications: List<NcNotification> = emptyList(),
    /** Count from the engine's listener — the badge, which arrives before the list. */
    val notificationCount: Int = 0,
    /** The notification tapped in the shade — scrolled to and highlighted once. */
    val focusedNotificationId: Int? = null,
    val notificationsLoading: Boolean = false,
    val notificationsError: String? = null,
    val shares: SharesPayload = SharesPayload(),
    val sharesLoading: Boolean = false,
    val sharesError: String? = null,
    val favorites: List<FileRow> = emptyList(),
    val favoritesLoading: Boolean = false,
    val favoritesError: String? = null,
    val trash: List<TrashItem> = emptyList(),
    val trashLoading: Boolean = false,
    val trashError: String? = null,
    /** Last pass that transferred something; survives later no-op polls. */
    val lastSynced: PairSyncedEvent? = null,
    /** When any pass last completed (ms), 0 = never. */
    val lastSyncAt: Long = 0L,
    val browsePath: String = "",
    val browseEntries: List<BrowseEntry> = emptyList(),
    val browseLoading: Boolean = false,

    // -- adding a folder: the remote pick, and where it lands locally --------
    /** The core's sync root; the default parent for a newly added folder. */
    val baseDir: String = "",
    /** Remote folder chosen in the remote picker, awaiting confirmation. */
    val pendingRemoteRoot: String = "",
    /** Absolute local path the pending folder will sync to. */
    val pendingLocalDir: String = "",
    /** Current directory in the local filesystem browser. */
    val localPath: String = "",
    val localDirs: List<LocalDir> = emptyList(),
    val localCanGoUp: Boolean = false,
    val busy: Boolean = false,
    val message: String? = null,          // transient snackbar text
    val hasAllFiles: Boolean = false,
    val hasNotifications: Boolean = false,
    val ignoringBattery: Boolean = false,
)
