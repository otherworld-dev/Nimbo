/*
 * FilesRepository.kt — the one place that answers "what is in this folder, and
 * which of it is actually on my phone?".
 *
 * The remote listing, the configured sync pairs, the local filesystem and the
 * engine's conflict set are four different sources; joining them here means every
 * screen renders a single FileRow and never has to know that.
 */
package dev.otherworld.nimbo.core

import android.util.Log
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import java.io.File

/** One row in the file browser: server metadata plus what we know locally. */
data class FileRow(
    val remotePath: String,
    val name: String,
    val isDir: Boolean,
    val size: Long,
    val modified: String,
    val fileId: String,
    val contentType: String,
    val syncState: SyncState,
    /** Absolute path on this device, when the file is genuinely there. */
    val localPath: String?,
    val isFavorite: Boolean = false,
) {
    /** True when the file can be opened with no network at all. */
    val isOffline: Boolean get() = syncState == SyncState.SYNCED && localPath != null

    /**
     * Whether it is worth asking the server for a thumbnail. Nextcloud also
     * previews PDFs and office documents, but images are the case that makes
     * a file list feel like one, and asking for the rest costs a round trip
     * per row to usually be told no.
     */
    val isPreviewable: Boolean
        get() = !isDir && fileId.isNotBlank() &&
            (contentType.startsWith("image/") || contentType == "application/pdf")
}

object FilesRepository {

    private const val TAG = "FilesRepository"

    /**
     * How many search hits to ask for. A result set of exactly this size is
     * indistinguishable from a truncated one, so callers must say so rather
     * than present it as the complete answer.
     */
    const val SEARCH_LIMIT = 50

    /**
     * Lists [remotePath] ("" is the account root), classified against the local
     * side. Directories sort first, then case-insensitive by name — the order
     * every file manager uses, applied here so no screen has to re-sort.
     */
    suspend fun list(remotePath: String): Result<List<FileRow>> = withContext(Dispatchers.IO) {
        NimboCore.browse(remotePath).map { entries ->
            val here = remotePath.trim().trim('/')
            val pairs = NimboCore.pairs.value
            val conflicts = conflictedRemotePaths()

            entries
                // A depth-1 listing includes the directory itself.
                .filter { it.path.trim().trim('/') != here }
                .map { entry -> toRow(entry, pairs, conflicts) }
                .sortedWith(compareByDescending<FileRow> { it.isDir }
                    .thenBy(String.CASE_INSENSITIVE_ORDER) { it.name })
        }
    }

    /**
     * The user's favourites, classified against the local side exactly as a
     * directory listing is — a starred file that is synced should say so.
     *
     * No self-filtering here: the server's REPORT returns only the starred
     * items, never the collection they were asked about.
     */
    suspend fun favorites(): Result<List<FileRow>> = withContext(Dispatchers.IO) {
        NimboCore.favorites().map { entries ->
            val pairs = NimboCore.pairs.value
            val conflicts = conflictedRemotePaths()
            entries
                .filter { it.path.trim().trim('/').isNotEmpty() }
                .map { entry -> toRow(entry, pairs, conflicts) }
                .sortedWith(compareByDescending<FileRow> { it.isDir }
                    .thenBy(String.CASE_INSENSITIVE_ORDER) { it.name })
        }
    }

    /**
     * Search hits, classified against the local side like any listing so the
     * sync badges mean the same thing they do in a folder.
     */
    suspend fun search(term: String): Result<List<FileRow>> = withContext(Dispatchers.IO) {
        NimboCore.search(term, SEARCH_LIMIT).map { entries ->
            val pairs = NimboCore.pairs.value
            val conflicts = conflictedRemotePaths()
            entries
                .filter { it.path.trim().trim('/').isNotEmpty() }
                .map { entry -> toRow(entry, pairs, conflicts) }
                .sortedWith(compareByDescending<FileRow> { it.isDir }
                    .thenBy(String.CASE_INSENSITIVE_ORDER) { it.name })
        }
    }

    private fun toRow(
        entry: BrowseEntry,
        pairs: List<SyncPair>,
        conflicts: Set<String>,
    ): FileRow {
        val path = entry.path.trim().trim('/')
        val local = localPathFor(path, pairs)
        val state = syncStateFor(
            remotePath = path,
            pairs = pairs,
            existsLocally = { candidate -> runCatching { File(candidate).exists() }.getOrDefault(false) },
            conflictedPaths = conflicts,
        )
        return FileRow(
            remotePath = path,
            name = entry.name.ifBlank { path.substringAfterLast('/') },
            isDir = entry.isDir,
            size = entry.size,
            modified = entry.lastModified,
            fileId = entry.fileID,
            contentType = entry.contentType,
            syncState = state,
            localPath = local?.takeIf { state == SyncState.SYNCED },
            isFavorite = entry.isFavorite,
        )
    }

    /**
     * Conflicts as account-relative remote paths. Empty under the v1 auto policy,
     * so a failure here must not fail the listing — an unmarked conflict is a far
     * smaller problem than a folder that will not open.
     */
    private suspend fun conflictedRemotePaths(): Set<String> =
        NimboCore.conflicts().getOrElse { error ->
            Log.w(TAG, "could not read conflicts; listing without them", error)
            emptyList()
        }.mapNotNullTo(mutableSetOf()) { item ->
            val root = item.remoteRoot.trim().trim('/')
            val rel = item.path.trim().trim('/')
            when {
                rel.isEmpty() -> null
                root.isEmpty() -> rel
                else -> "$root/$rel"
            }
        }
}
