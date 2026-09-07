package dev.otherworld.nimbo.core

/*
 * The engine's status line is per-account, so "Up to date" can be true of one
 * folder while another has stopped syncing. These pin the payload and the rule
 * the UI turns it into: never claim everything is fine while a folder is failing.
 */

import kotlinx.serialization.decodeFromString
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class FailingFolderTest {

    @Test
    fun `a failing folder decodes every pinned field`() {
        val json = """
            [{"localDir":"/sd/Nimbo/Photos",
              "lastError":"local folder is missing or empty",
              "since":"2026-08-28T01:53:00Z"}]
        """.trimIndent()

        val failing = NimboJson.decodeFromString<List<FailingFolder>>(json)

        assertEquals(1, failing.size)
        assertEquals("/sd/Nimbo/Photos", failing[0].localDir)
        assertEquals("local folder is missing or empty", failing[0].lastError)
        assertEquals("2026-08-28T01:53:00Z", failing[0].since)
    }

    @Test
    fun `all healthy is an empty list`() {
        assertTrue(NimboJson.decodeFromString<List<FailingFolder>>("[]").isEmpty())
    }

    @Test
    fun `a sparse entry still decodes`() {
        val f = NimboJson.decodeFromString<List<FailingFolder>>("""[{"localDir":"/sd/x"}]""")
        assertEquals("/sd/x", f[0].localDir)
        assertEquals("", f[0].lastError)
    }

    @Test
    fun `the display name is the folder leaf`() {
        assertEquals("Photos", FailingFolder(localDir = "/sd/Nimbo/Photos").name)
    }

    // -- the honesty rule ----------------------------------------------------

    @Test
    fun `the status is not up to date while any folder is failing`() {
        val failing = listOf(FailingFolder(localDir = "/sd/Photos", lastError = "boom"))
        assertFalse(isEverythingHealthy("Up to date", failing))
    }

    @Test
    fun `the status stands when every folder is healthy`() {
        assertTrue(isEverythingHealthy("Up to date", emptyList()))
    }

    /** One failure is enough to stop claiming everything is fine. */
    @Test
    fun `one failure among many folders still breaks the claim`() {
        val failing = listOf(FailingFolder(localDir = "/sd/One", lastError = "boom"))
        assertFalse(isEverythingHealthy("Up to date", failing))
    }

    @Test
    fun `the summary names a single failing folder`() {
        val one = listOf(FailingFolder(localDir = "/sd/Photos", lastError = "boom"))
        assertEquals("Photos isn't syncing", failingSummary(one))
    }

    @Test
    fun `the summary counts several failing folders`() {
        val many = listOf(
            FailingFolder(localDir = "/sd/Photos"),
            FailingFolder(localDir = "/sd/Docs"),
        )
        assertEquals("2 folders aren't syncing", failingSummary(many))
    }

    @Test
    fun `no failures has no summary`() {
        assertEquals(null, failingSummary(emptyList()))
    }
}
