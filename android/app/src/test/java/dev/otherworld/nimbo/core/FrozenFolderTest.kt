package dev.otherworld.nimbo.core

/*
 * FrozenFoldersJSON's field names are pinned in MOBILE_API.md. A mismatch here
 * does not throw — it decodes to blanks, and the resume banner would show an
 * empty folder name with no reason, which is worse than not showing it at all.
 */

import kotlinx.serialization.decodeFromString
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

class FrozenFolderTest {

    @Test
    fun `a frozen folder decodes every pinned field`() {
        val json = """
            [{"localDir":"/storage/emulated/0/Nimbo/Photos",
              "reason":"the server listing came back empty",
              "sample":["Photos/a.jpg","Photos/b.jpg"]}]
        """.trimIndent()

        val frozen = NimboJson.decodeFromString<List<FrozenFolder>>(json)

        assertEquals(1, frozen.size)
        assertEquals("/storage/emulated/0/Nimbo/Photos", frozen[0].localDir)
        assertEquals("the server listing came back empty", frozen[0].reason)
        assertEquals(listOf("Photos/a.jpg", "Photos/b.jpg"), frozen[0].sample)
    }

    /** Nothing paused is the normal case and must not look like an error. */
    @Test
    fun `an empty array is no frozen folders`() {
        assertTrue(NimboJson.decodeFromString<List<FrozenFolder>>("[]").isEmpty())
    }

    /** A payload missing optional fields must still decode rather than throw. */
    @Test
    fun `a sparse entry falls back to defaults`() {
        val frozen = NimboJson.decodeFromString<List<FrozenFolder>>("""[{"localDir":"/sd/x"}]""")
        assertEquals("/sd/x", frozen[0].localDir)
        assertEquals("", frozen[0].reason)
        assertTrue(frozen[0].sample.isEmpty())
    }

    /** The folder name shown in the banner is the leaf, not the whole path. */
    @Test
    fun `the display name is the last path segment`() {
        assertEquals("Photos", FrozenFolder(localDir = "/storage/emulated/0/Nimbo/Photos").name)
        assertEquals("sd", FrozenFolder(localDir = "/sd").name)
    }

    /** A path with no leaf still has to render as something. */
    @Test
    fun `a degenerate path falls back to the path itself`() {
        assertEquals("/", FrozenFolder(localDir = "/").name)
        assertEquals("", FrozenFolder(localDir = "").name)
    }
}
