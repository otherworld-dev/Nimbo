/*
 * FileOpener.kt — turning a server file into something another app can open.
 *
 * A file already synced to this device is opened straight from disk with no
 * network at all; anything else is downloaded into the cache first. Either way
 * it is handed out as a content:// URI through the FileProvider, because Android
 * rejects file:// URIs in intents.
 */
package dev.otherworld.nimbo.platform

import android.content.Context
import android.net.Uri
import androidx.core.content.FileProvider
import dev.otherworld.nimbo.core.FileRow
import dev.otherworld.nimbo.core.NimboCore
import dev.otherworld.nimbo.core.cacheRelativePathFor
import dev.otherworld.nimbo.core.mimeTypeFor
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import java.io.File

/** A file ready to hand to another app. */
data class OpenableFile(val uri: Uri, val mimeType: String, val cameFromCache: Boolean)

object FileOpener {

    private fun authority(context: Context) = context.packageName + ".files"

    /**
     * Resolves [row] to something openable, downloading it only when it is not
     * already on the device.
     *
     * The download has no deadline (see MOBILE_API.md), so this suspends for as
     * long as the transfer takes — call it somewhere a long wait is expected and
     * visible to the user.
     */
    suspend fun openable(context: Context, row: FileRow): Result<OpenableFile> =
        withContext(Dispatchers.IO) {
            runCatching {
                val mime = mimeTypeFor(row.name, row.contentType)

                // Already synced: open in place, no network, works offline.
                val local = row.localPath?.let(::File)
                if (local != null && local.isFile) {
                    return@runCatching OpenableFile(uriFor(context, local), mime, false)
                }

                val cached = File(context.cacheDir, cacheRelativePathFor(row.fileId, row.name))
                if (!cached.isFile || cached.length() == 0L) {
                    NimboCore.downloadToFile(row.remotePath, cached.absolutePath).getOrThrow()
                }
                OpenableFile(uriFor(context, cached), mime, true)
            }
        }

    private fun uriFor(context: Context, file: File): Uri =
        FileProvider.getUriForFile(context, authority(context), file)

    /** Drops everything previously downloaded for viewing. */
    fun clearCache(context: Context) {
        runCatching { File(context.cacheDir, "files").deleteRecursively() }
    }
}
