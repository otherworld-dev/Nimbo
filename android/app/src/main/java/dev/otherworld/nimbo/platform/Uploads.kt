/*
 * Uploads.kt — bridging the Android document picker to the Go facade.
 *
 * The picker hands back a content:// URI, which the engine cannot open: it works
 * in POSIX paths. So the bytes are staged into the cache first and the staged
 * copy is what gets uploaded, then deleted.
 */
package dev.otherworld.nimbo.platform

import android.content.Context
import android.net.Uri
import android.provider.OpenableColumns
import android.util.Log
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import java.io.File

private const val TAG = "Uploads"

/** A picked document copied somewhere the engine can read it. */
data class StagedUpload(val file: File, val displayName: String) {
    fun discard() {
        runCatching { file.delete() }
    }
}

object Uploads {

    /**
     * Copies the picked document into the cache.
     *
     * The display name is asked of the content provider and falls back to the
     * URI's last segment; either way it is sanitised, because it becomes both a
     * local filename and the name on the server.
     */
    suspend fun stage(context: Context, uri: Uri): Result<StagedUpload> =
        withContext(Dispatchers.IO) {
            runCatching {
                val name = displayName(context, uri)
                val target = File(context.cacheDir, "uploads/$name").apply {
                    parentFile?.mkdirs()
                }
                context.contentResolver.openInputStream(uri).use { input ->
                    requireNotNull(input) { "Could not read the selected file" }
                    target.outputStream().use { output -> input.copyTo(output) }
                }
                StagedUpload(target, name)
            }
        }

    private fun displayName(context: Context, uri: Uri): String {
        val fromProvider = runCatching {
            context.contentResolver.query(uri, null, null, null, null)?.use { cursor ->
                val column = cursor.getColumnIndex(OpenableColumns.DISPLAY_NAME)
                if (column >= 0 && cursor.moveToFirst()) cursor.getString(column) else null
            }
        }.onFailure { Log.w(TAG, "could not read the display name", it) }.getOrNull()

        val raw = fromProvider ?: uri.lastPathSegment ?: "upload"
        // Reuse the download sanitiser: the picker's name is no more trustworthy
        // than the server's, and this one is about to become a path AND a
        // server-side filename.
        return dev.otherworld.nimbo.core.cacheRelativePathFor("x", raw).substringAfterLast('/')
    }
}
