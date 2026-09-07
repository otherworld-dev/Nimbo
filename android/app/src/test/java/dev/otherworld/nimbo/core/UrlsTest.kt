package dev.otherworld.nimbo.core

/*
 * Nextcloud's OCS navigation API (core/navigation/apps) returns each app's href
 * relative to the server root — "/index.php/apps/files/". Handed to Uri.parse()
 * as-is it yields a URI with no scheme and no host, which a Custom Tab cannot
 * open, so every app tile did nothing.
 *
 * The desktop client has always resolved these against the account's server URL
 * (cmd/nimbo-gui/service.go absURL); these tests pin the same behaviour here,
 * including the no-op cases so an already-absolute href is never mangled.
 */

import org.junit.Assert.assertEquals
import org.junit.Test

class UrlsTest {

    private val server = "https://cloud.example.com"

    @Test
    fun `a root-relative href is resolved against the server`() {
        assertEquals(
            "https://cloud.example.com/index.php/apps/files/",
            absoluteUrl(server, "/index.php/apps/files/"),
        )
    }

    @Test
    fun `an href without a leading slash is still resolved`() {
        assertEquals(
            "https://cloud.example.com/index.php/apps/notes",
            absoluteUrl(server, "index.php/apps/notes"),
        )
    }

    @Test
    fun `a trailing slash on the server does not double up`() {
        assertEquals(
            "https://cloud.example.com/index.php/apps/deck",
            absoluteUrl("https://cloud.example.com/", "/index.php/apps/deck"),
        )
    }

    @Test
    fun `an already-absolute href is left alone`() {
        val absolute = "https://other.example.com/index.php/apps/talk"
        assertEquals(absolute, absoluteUrl(server, absolute))
    }

    @Test
    fun `a server on a sub-path is preserved`() {
        assertEquals(
            "https://example.com/nextcloud/index.php/apps/files",
            absoluteUrl("https://example.com/nextcloud", "/index.php/apps/files"),
        )
    }

    @Test
    fun `a blank href stays blank`() {
        assertEquals("", absoluteUrl(server, ""))
    }

    /** Without a server URL there is nothing to resolve against — do not invent one. */
    @Test
    fun `a blank server leaves the href untouched`() {
        assertEquals("/index.php/apps/files/", absoluteUrl("", "/index.php/apps/files/"))
    }
}
