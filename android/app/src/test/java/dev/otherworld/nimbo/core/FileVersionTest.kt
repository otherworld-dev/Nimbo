package dev.otherworld.nimbo.core

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * A version list is a list of dates — if the date is wrong or blank, the screen
 * is useless, because the date is the only thing distinguishing one revision
 * from another.
 */
class FileVersionTest {

    @Test
    fun `decodes the untagged PascalCase payload`() {
        val json = """[{"Href":"/remote.php/dav/versions/adam/versions/12/1690000000",
                        "Modified":"2026-08-20T14:05:00Z","Size":2048}]"""
        val versions = NimboJson.decodeFromString<List<FileVersion>>(json)
        val v = versions.single()
        assertEquals(2048L, v.size)
        assertTrue(v.href.endsWith("1690000000"))
    }

    @Test
    fun `a missing or unparseable date degrades to something honest`() {
        // Never show "1 Jan 1970" for a date the server did not give us.
        assertEquals("Unknown date", FileVersion(modified = "").whenLabel)
        assertEquals("Unknown date", FileVersion(modified = "not a date").whenLabel)
    }

    @Test
    fun `a real timestamp is shown as a date and time`() {
        val label = FileVersion(modified = "2026-08-20T14:05:00Z").whenLabel
        // Rendered in the device's zone, so assert on content rather than an
        // exact string: the year and the month must survive.
        assertTrue("got: $label", label.contains("2026"))
        assertTrue("got: $label", label.contains("Aug"))
    }

    @Test
    fun `versions are ordered newest first regardless of what the server sent`() {
        val out = listOf(
            FileVersion(href = "a", modified = "2026-08-01T00:00:00Z"),
            FileVersion(href = "c", modified = "2026-08-20T00:00:00Z"),
            FileVersion(href = "b", modified = "2026-08-10T00:00:00Z"),
        ).newestFirst()
        assertEquals(listOf("c", "b", "a"), out.map { it.href })
    }

    @Test
    fun `an undated version sorts last rather than first`() {
        // Unknown is not "the beginning of time"; putting it on top would
        // present it as the most recent revision.
        val out = listOf(
            FileVersion(href = "unknown", modified = ""),
            FileVersion(href = "dated", modified = "2020-01-01T00:00:00Z"),
        ).newestFirst()
        assertEquals(listOf("dated", "unknown"), out.map { it.href })
    }
}
