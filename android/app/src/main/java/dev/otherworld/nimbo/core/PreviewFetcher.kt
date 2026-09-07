/*
 * PreviewFetcher.kt — teaches Coil to load thumbnails through the Go facade.
 *
 * Previews could be fetched over plain HTTP from Kotlin, but that would mean a
 * second authenticated path with its own copy of the app password and its own
 * timeout behaviour. Routing them through the engine keeps one transport, one
 * set of credentials and the user's bandwidth limits.
 */
package dev.otherworld.nimbo.core

import android.content.Context
import coil.ImageLoader
import coil.decode.DataSource
import coil.decode.ImageSource
import coil.fetch.FetchResult
import coil.fetch.Fetcher
import coil.fetch.SourceResult
import coil.request.Options
import okio.Buffer

/**
 * Coil model for a server-rendered preview. Equality includes the size so a grid
 * and a detail view do not fight over one cache entry.
 */
data class PreviewRequest(val fileId: String, val px: Int = 256)

internal class PreviewFetcher(
    private val request: PreviewRequest,
    private val options: Options,
) : Fetcher {

    override suspend fun fetch(): FetchResult? {
        if (request.fileId.isBlank()) return null
        // A file with no preview is ordinary, not a fault: returning null lets
        // Coil fall back to the caller's placeholder without logging an error.
        val bytes = NimboCore.preview(request.fileId, request.px).getOrNull() ?: return null
        if (bytes.isEmpty()) return null
        return SourceResult(
            source = ImageSource(Buffer().apply { write(bytes) }, options.context),
            mimeType = null,
            dataSource = DataSource.NETWORK,
        )
    }

    class Factory : Fetcher.Factory<PreviewRequest> {
        override fun create(data: PreviewRequest, options: Options, imageLoader: ImageLoader) =
            PreviewFetcher(data, options)
    }
}

/** The app's shared loader, taught about [PreviewRequest]. */
fun nimboImageLoader(context: Context): ImageLoader =
    ImageLoader.Builder(context)
        .components { add(PreviewFetcher.Factory()) }
        .build()
