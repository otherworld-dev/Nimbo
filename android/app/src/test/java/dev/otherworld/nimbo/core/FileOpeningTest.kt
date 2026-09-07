package dev.otherworld.nimbo.core

/*
 * Opening a server file means writing it into the cache and handing it to
 * another app. Both halves are attacker-adjacent: the file name comes from the
 * server, so it must never steer the write out of the cache directory, and the
 * MIME type decides which app is handed the content.
 */

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Test

class FileOpeningTest {

    // -- cache paths ---------------------------------------------------------

    @Test
    fun `a download is cached under its file id`() {
        assertEquals("files/12345/holiday.jpg", cacheRelativePathFor("12345", "holiday.jpg"))
    }

    /** A server-supplied name must not be able to climb out of the cache. */
    @Test
    fun `path separators and dot-dot are stripped from the name`() {
        val path = cacheRelativePathFor("12345", "../../etc/passwd")
        assertFalse("escaped the cache dir: $path", path.contains(".."))
        assertEquals("files/12345/etc_passwd", path)
    }

    @Test
    fun `a hostile file id cannot climb either`() {
        val path = cacheRelativePathFor("../../..", "x.txt")
        assertFalse("escaped the cache dir: $path", path.contains(".."))
    }

    @Test
    fun `a blank name still yields a usable path`() {
        assertEquals("files/12345/file", cacheRelativePathFor("12345", ""))
    }

    @Test
    fun `a blank file id falls back to a stable bucket`() {
        assertEquals("files/unknown/holiday.jpg", cacheRelativePathFor("", "holiday.jpg"))
    }

    // -- mime types ----------------------------------------------------------

    @Test
    fun `the server content type is preferred`() {
        assertEquals("image/png", mimeTypeFor("holiday.png", "image/png"))
    }

    /** Nextcloud falls back to octet-stream a lot; the extension is better than that. */
    @Test
    fun `octet-stream defers to the extension`() {
        assertEquals("image/jpeg", mimeTypeFor("holiday.jpg", "application/octet-stream"))
        assertEquals("application/pdf", mimeTypeFor("manual.pdf", "application/octet-stream"))
    }

    @Test
    fun `a missing content type falls back to the extension`() {
        assertEquals("video/mp4", mimeTypeFor("clip.mp4", ""))
        assertEquals("text/markdown", mimeTypeFor("notes.md", ""))
    }

    @Test
    fun `an unknown extension is left open to any app`() {
        assertEquals("*/*", mimeTypeFor("archive.zzz", ""))
    }

    @Test
    fun `the extension check is case-insensitive`() {
        assertEquals("image/jpeg", mimeTypeFor("HOLIDAY.JPG", ""))
    }
}
