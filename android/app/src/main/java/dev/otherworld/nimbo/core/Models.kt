/*
 * Models.kt — the Kotlin shapes of every JSON payload that crosses the gomobile
 * boundary, plus the shared lenient Json parser.
 *
 * Rules (see MOBILE_API.md): every field has a default so a missing key can
 * never throw, unknown keys are ignored, and the PascalCase payloads (untagged
 * Go structs) are modelled with explicit @SerialName. Nothing in here touches
 * Android or the Go core — it is pure data, consumed by every other slice.
 */
package dev.otherworld.nimbo.core

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json
import java.time.Instant
import java.time.ZoneId
import java.time.format.DateTimeFormatter
import java.util.Locale

/** The one parser used for every payload from the core. Total: never throws on shape drift. */
val NimboJson: Json = Json {
    ignoreUnknownKeys = true
    isLenient = true
    coerceInputValues = true
    explicitNulls = false
}

// ---------------------------------------------------------------------------
// camelCase payloads (tagged Go structs)
// ---------------------------------------------------------------------------

@Serializable
data class SyncProgress(
    val active: Boolean = false,
    val current: String = "",
    val done: Long = 0,
    val total: Long = 0,
    val speed: Long = 0,
    val avgSpeed: Long = 0,
    val doneBytes: Long = 0,
    val totalBytes: Long = 0,
    val enumerating: Boolean = false,
)

@Serializable
data class SyncPair(
    val localDir: String = "",
    val remoteRoot: String = "",
    val excludes: List<String> = emptyList(),
)

@Serializable
data class NcAccount(
    val id: String = "",
    val serverURL: String = "",
    val loginName: String = "",
)

@Serializable
data class Quota(
    val free: Long = 0,
    val used: Long = 0,
    val total: Long = 0,
    val relative: Double = 0.0,
    val quota: Long = 0,
)

@Serializable
data class NcApp(
    val id: String = "",
    val name: String = "",
    val href: String = "",
    val icon: String = "",
)

// ---------------------------------------------------------------------------
// PascalCase payloads (untagged Go structs — field names are exact)
// ---------------------------------------------------------------------------

@Serializable
data class SyncStats(
    @SerialName("Downloaded") val downloaded: Long = 0,
    @SerialName("Uploaded") val uploaded: Long = 0,
    @SerialName("MkLocal") val mkLocal: Long = 0,
    @SerialName("MkRemote") val mkRemote: Long = 0,
    @SerialName("DelLocal") val delLocal: Long = 0,
    @SerialName("DelRemote") val delRemote: Long = 0,
    @SerialName("Moved") val moved: Long = 0,
    @SerialName("Conflicts") val conflicts: Long = 0,
    @SerialName("ConflictsIdentical") val conflictsIdentical: Long = 0,
    @SerialName("ConflictsResurrected") val conflictsResurrected: Long = 0,
    @SerialName("Failed") val failed: Long = 0,
)

/**
 * True when the pass actually did something.
 *
 * The engine reports every completed pass, including the no-op poll that follows
 * a real one moments later. Only a pass with activity may replace what the UI
 * shows as the last sync — otherwise a genuine "173 uploaded" is overwritten by
 * zeros seconds later and the transfer looks like it never happened.
 */
val SyncStats.hasActivity: Boolean
    get() = downloaded > 0 || uploaded > 0 ||
        mkLocal > 0 || mkRemote > 0 ||
        delLocal > 0 || delRemote > 0 ||
        moved > 0 ||
        conflicts > 0 || conflictsIdentical > 0 || conflictsResurrected > 0 ||
        failed > 0

/**
 * A WebDAV listing entry. `Checksums` is deliberately not modelled (its shape is
 * not pinned) — ignoreUnknownKeys covers it.
 */
@Serializable
data class BrowseEntry(
    @SerialName("Path") val path: String = "",
    @SerialName("IsDir") val isDir: Boolean = false,
    @SerialName("Size") val size: Long = 0,
    @SerialName("ETag") val etag: String = "",
    @SerialName("FileID") val fileID: String = "",
    @SerialName("LastModified") val lastModified: String = "",
    @SerialName("ContentType") val contentType: String = "",
    /** oc:favorite. False also when the server does not report it — never a claim. */
    @SerialName("IsFavorite") val isFavorite: Boolean = false,
) {
    /** Last path segment, e.g. "Photos" for "/Documents/Photos/". */
    val name: String get() = path.trimEnd('/').substringAfterLast('/')
}

@Serializable
data class Diagnostics(
    @SerialName("ServerURL") val serverURL: String = "",
    @SerialName("ServerVersion") val serverVersion: String = "",
    @SerialName("Account") val account: String = "",
    @SerialName("PushAvailable") val pushAvailable: Boolean = false,
    @SerialName("PushConnected") val pushConnected: Boolean = false,
    @SerialName("PushSince") val pushSince: String = "",
    @SerialName("LastStatus") val lastStatus: String = "",
    @SerialName("LastSyncAt") val lastSyncAt: String = "",
)

// ---------------------------------------------------------------------------
/**
 * A deferred conflict awaiting a choice. Empty under the v1 auto policy (which
 * keeps both sides), but the file browser marks these when they appear.
 * PascalCase: the Go struct is untagged.
 */
@Serializable
data class ConflictItem(
    @SerialName("LocalDir") val localDir: String = "",
    @SerialName("RemoteRoot") val remoteRoot: String = "",
    /** Pair-relative, so the account-relative path is remoteRoot + "/" + path. */
    @SerialName("Path") val path: String = "",
    @SerialName("Kind") val kind: String = "",
    @SerialName("LocalExists") val localExists: Boolean = false,
    @SerialName("RemoteExists") val remoteExists: Boolean = false,
    @SerialName("LocalSize") val localSize: Long = 0,
    @SerialName("RemoteSize") val remoteSize: Long = 0,
)

/**
 * One item in the server trashbin.
 *
 * PascalCase: the Go struct is untagged. [href] is an opaque handle for restore
 * and permanent-delete — never construct or parse it.
 */
@Serializable
data class TrashItem(
    @SerialName("Href") val href: String = "",
    @SerialName("Name") val name: String = "",
    /** Files-root-relative path it was deleted from. */
    @SerialName("OriginalLocation") val originalLocation: String = "",
    /** RFC 3339. */
    @SerialName("DeletedAt") val deletedAt: String = "",
    @SerialName("Size") val size: Long = 0,
    @SerialName("IsDir") val isDir: Boolean = false,
) {
    /**
     * The folder it was deleted from — what the user recognises. Items deleted
     * from the account root report "Files", matching how Nextcloud names it.
     */
    val originalFolder: String
        get() {
            val path = originalLocation.trim().trim('/')
            if (!path.contains('/')) return "Files"
            return path.substringBeforeLast('/').substringAfterLast('/')
        }
}

/**
 * A previous revision of a file, from Nextcloud's versions app.
 * PascalCase: the Go struct is untagged. [href] is an opaque handle for restore.
 */
@Serializable
data class FileVersion(
    @SerialName("Href") val href: String = "",
    /** RFC 3339. Empty or unparseable when the server did not say. */
    @SerialName("Modified") val modified: String = "",
    @SerialName("Size") val size: Long = 0,
) {
    /** The instant this revision was stored, or null if the server gave none. */
    val instant: Instant?
        get() = runCatching { Instant.parse(modified.trim()) }.getOrNull()

    /**
     * When it was saved, in the device's own zone. A version list is a list of
     * dates, so a missing one says so rather than falling back to the epoch —
     * "1 Jan 1970" would read as a real revision from a real time.
     */
    val whenLabel: String
        get() {
            val at = instant ?: return "Unknown date"
            return VERSION_DATE_FORMAT.format(at.atZone(ZoneId.systemDefault()))
        }
}

private val VERSION_DATE_FORMAT: DateTimeFormatter =
    DateTimeFormatter.ofPattern("d MMM yyyy, HH:mm", Locale.UK)

/**
 * Newest first, with undated revisions last. Unknown is not the beginning of
 * time: sorting a dateless version to the top would present it as the most
 * recent one.
 */
fun List<FileVersion>.newestFirst(): List<FileVersion> =
    sortedWith(compareByDescending { it.instant ?: Instant.MIN })

/**
 * One server notification. Server-style snake_case — this payload is the OCS
 * response shape, not one of ours.
 */
@Serializable
data class NcNotification(
    @SerialName("notification_id") val id: Int = 0,
    val app: String = "",
    val subject: String = "",
    val message: String = "",
    val link: String = "",
    @SerialName("object_type") val objectType: String = "",
    @SerialName("datetime") val time: String = "",
    val actions: List<NcNotificationAction> = emptyList(),
) {
    /** RFC 3339 when the server gave one. */
    val instant: Instant?
        get() = runCatching { Instant.parse(time.trim()) }.getOrNull()

    /**
     * When it arrived, in the device's zone — or nothing at all. A notification
     * with no usable date shows no date rather than a fabricated one.
     */
    val whenLabel: String
        get() = instant?.let { NOTIFICATION_DATE_FORMAT.format(it.atZone(ZoneId.systemDefault())) } ?: ""
}

/**
 * A button the notification itself offered. [link] and [type] are the server's
 * own values and are passed back verbatim — never constructed.
 */
@Serializable
data class NcNotificationAction(
    val label: String = "",
    val link: String = "",
    /** The HTTP method to call [link] with; empty means GET. */
    val type: String = "",
    val primary: Boolean = false,
)

private val NOTIFICATION_DATE_FORMAT: DateTimeFormatter =
    DateTimeFormatter.ofPattern("d MMM, HH:mm", Locale.UK)

/**
 * A share this account takes part in, straight from the OCS Sharing API
 * (snake_case, unlike everything else that crosses the boundary).
 *
 * `id` arrives as a string here because the server types it inconsistently
 * across versions and the Go side normalises it before we see it.
 */
@Serializable
data class Share(
    val id: String = "",
    @SerialName("share_type") val shareType: Int = -1,
    /** "file" or "folder" — the only way to know which a shared path is. */
    @SerialName("item_type") val itemType: String = "",
    /** Account-relative for an own share; a path in the OWNER's account for a received one. */
    val path: String = "",
    val permissions: Int = 0,
    @SerialName("share_with") val shareWith: String = "",
    /** The public link, for link shares. */
    val url: String = "",
    val token: String = "",
    /** "YYYY-MM-DD", or empty for no expiry. */
    val expiration: String = "",
    @SerialName("uid_owner") val owner: String = "",
    @SerialName("displayname_owner") val ownerDisplay: String = "",
) {
    /** Last path segment — a share row names the file, not its whole path. */
    val name: String
        get() = path.trim().trim('/').substringAfterLast('/').ifBlank { "Files" }

    /** The folder it lives in, as [Share.name]'s counterpart. */
    val folder: String
        get() {
            val clean = path.trim().trim('/')
            return if (clean.contains('/')) clean.substringBeforeLast('/') else "Files"
        }

    /**
     * What kind of share this is. An unrecognised type still gets a truthful
     * label: the app knowing nothing about it does not make it not a share.
     */
    val kindLabel: String
        get() = when (shareType) {
            SHARE_TYPE_USER -> "Person"
            SHARE_TYPE_GROUP -> "Group"
            SHARE_TYPE_LINK -> "Link"
            SHARE_TYPE_EMAIL -> "Email"
            SHARE_TYPE_FEDERATED, SHARE_TYPE_FEDERATED_GROUP -> "Another server"
            SHARE_TYPE_CIRCLE -> "Team"
            SHARE_TYPE_ROOM -> "Conversation"
            else -> "Shared"
        }

    /** Who can reach it. A link has no recipient, so it says what it actually means. */
    val recipientLabel: String
        get() = when {
            isLink -> "Anyone with the link"
            shareWith.isNotBlank() -> shareWith
            else -> "Someone"
        }

    /** Who shared it — for the things other people shared with this account. */
    val sharedBy: String
        get() = ownerDisplay.ifBlank { owner.ifBlank { "Someone" } }

    val isFolder: Boolean get() = itemType.equals("folder", ignoreCase = true)

    val isLink: Boolean get() = shareType == SHARE_TYPE_LINK || shareType == SHARE_TYPE_EMAIL

    val hasExpiry: Boolean get() = expiration.isNotBlank()

    companion object {
        const val SHARE_TYPE_USER = 0
        const val SHARE_TYPE_GROUP = 1
        const val SHARE_TYPE_LINK = 3
        const val SHARE_TYPE_EMAIL = 4
        const val SHARE_TYPE_FEDERATED = 6
        const val SHARE_TYPE_FEDERATED_GROUP = 9
        const val SHARE_TYPE_CIRCLE = 7
        const val SHARE_TYPE_ROOM = 10
    }
}

/**
 * The two halves of SharesJSON. They are kept apart because they mean opposite
 * things — one is what the user gave out, the other what they were given — and
 * a merged list could not say which is which.
 */
@Serializable
data class SharesPayload(
    val own: List<Share> = emptyList(),
    val received: List<Share> = emptyList(),
)

/**
 * A sync folder the damage guard has paused.
 *
 * The engine freezes a folder rather than applying a pass that would delete or
 * replace most of it. On a phone that is not always ransomware: a pair on shared
 * storage looks identical to a mass deletion when the volume fails to mount or
 * All-files access is withdrawn. camelCase per MOBILE_API.md.
 */
@Serializable
data class FrozenFolder(
    val localDir: String = "",
    /** The engine's own words for why it stopped — shown verbatim. */
    val reason: String = "",
    /** A few affected paths, so the user can judge whether the change was theirs. */
    val sample: List<String> = emptyList(),
) {
    /** Leaf of the local path: what the user calls this folder. */
    val name: String
        get() = localDir.trimEnd('/').substringAfterLast('/').ifBlank { localDir }
}

// Plain event types (not serialized — built from listener callbacks)
// ---------------------------------------------------------------------------

data class ToastEvent(
    val title: String,
    val message: String,
    val link: String,
)

data class PairSyncedEvent(
    val localDir: String,
    val remoteRoot: String,
    val stats: SyncStats,
    /** When we received it (wall clock, ms) — the engine sends no timestamp. */
    val atMillis: Long = 0L,
)
