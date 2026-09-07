/*
 * FileOpening.kt — where a downloaded file goes, and what type we tell other
 * apps it is.
 *
 * Both inputs come from the server, so both are sanitised here. Pure Kotlin so
 * the sanitising is actually tested rather than assumed.
 */
package dev.otherworld.nimbo.core

/** Characters that are legal in a name but not in a file path. */
private val UNSAFE_CHARS = Regex("""[:*?"<>|]+""")

/**
 * Flattens a server-supplied name into a single safe path segment.
 *
 * Splitting on separators and then dropping any part that is only dots is what
 * makes traversal impossible: ".." is discarded as a part rather than escaped as
 * text, so no combination of slashes and dots can climb out of the cache.
 */
private fun safeSegment(raw: String, fallback: String): String {
    val parts = raw.trim()
        .split('/', '\\')
        .map { it.replace(UNSAFE_CHARS, "_").trim() }
        .filter { part -> part.isNotEmpty() && !part.all { it == '.' } }
    return parts.joinToString("_").trim('_').ifEmpty { fallback }
}

/**
 * Cache-relative location for a downloaded file, keyed by its Nextcloud file id
 * so a changed file lands in the same place and a renamed one does not orphan.
 *
 * Callers resolve this against `context.cacheDir`; it is always a relative path
 * with no `..` segment, whatever the server called the file.
 */
fun cacheRelativePathFor(fileId: String, name: String): String {
    val bucket = safeSegment(fileId, "unknown")
    val leaf = safeSegment(name, "file")
    return "files/$bucket/$leaf"
}

/**
 * The MIME type to hand another app.
 *
 * The server's own content type wins when it says anything useful; Nextcloud
 * reports `application/octet-stream` often enough that the file extension is the
 * better guess in that case. A wildcard type lets the user pick the app when we
 * genuinely do not know.
 */
fun mimeTypeFor(name: String, contentType: String): String {
    val declared = contentType.trim().lowercase()
    if (declared.isNotEmpty() &&
        declared != "application/octet-stream" &&
        declared.contains('/')
    ) {
        return declared
    }
    val ext = name.substringAfterLast('.', "").lowercase()
    return EXTENSION_TYPES[ext] ?: "*/*"
}

private val EXTENSION_TYPES = mapOf(
    "jpg" to "image/jpeg", "jpeg" to "image/jpeg", "png" to "image/png",
    "gif" to "image/gif", "webp" to "image/webp", "heic" to "image/heic",
    "bmp" to "image/bmp", "svg" to "image/svg+xml",
    "mp4" to "video/mp4", "mkv" to "video/x-matroska", "mov" to "video/quicktime",
    "webm" to "video/webm", "avi" to "video/x-msvideo", "m4v" to "video/x-m4v",
    "mp3" to "audio/mpeg", "flac" to "audio/flac", "ogg" to "audio/ogg",
    "wav" to "audio/wav", "m4a" to "audio/mp4", "opus" to "audio/opus",
    "pdf" to "application/pdf",
    "txt" to "text/plain", "md" to "text/markdown", "csv" to "text/csv",
    "json" to "application/json", "xml" to "application/xml",
    "html" to "text/html", "htm" to "text/html",
    "zip" to "application/zip", "gz" to "application/gzip",
    "doc" to "application/msword",
    "docx" to "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
    "xls" to "application/vnd.ms-excel",
    "xlsx" to "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
    "ppt" to "application/vnd.ms-powerpoint",
    "pptx" to "application/vnd.openxmlformats-officedocument.presentationml.presentation",
    "odt" to "application/vnd.oasis.opendocument.text",
    "ods" to "application/vnd.oasis.opendocument.spreadsheet",
    "odp" to "application/vnd.oasis.opendocument.presentation",
)
