package dev.otherworld.nimbo.core

/*
 * TrashItem crosses the boundary as an untagged Go struct, so its field names
 * are PascalCase and a mismatch decodes to blanks rather than throwing — a trash
 * list of empty rows with dead restore buttons.
 */

import kotlinx.serialization.decodeFromString
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

class TrashItemTest {

    @Test
    fun `a trashed file decodes every pinned field`() {
        val json = """
            [{"Href":"/remote.php/dav/trashbin/adam/trash/notes.md.1234",
              "Name":"notes.md",
              "OriginalLocation":"Documents/notes.md",
              "DeletedAt":"2026-08-28T14:02:00Z",
              "Size":4096,
              "IsDir":false}]
        """.trimIndent()

        val items = NimboJson.decodeFromString<List<TrashItem>>(json)

        assertEquals(1, items.size)
        assertEquals("/remote.php/dav/trashbin/adam/trash/notes.md.1234", items[0].href)
        assertEquals("notes.md", items[0].name)
        assertEquals("Documents/notes.md", items[0].originalLocation)
        assertEquals("2026-08-28T14:02:00Z", items[0].deletedAt)
        assertEquals(4096L, items[0].size)
        assertTrue(!items[0].isDir)
    }

    @Test
    fun `an empty trashbin is an empty list`() {
        assertTrue(NimboJson.decodeFromString<List<TrashItem>>("[]").isEmpty())
    }

    @Test
    fun `a sparse entry still decodes`() {
        val items = NimboJson.decodeFromString<List<TrashItem>>("""[{"Name":"x.txt"}]""")
        assertEquals("x.txt", items[0].name)
        assertEquals("", items[0].href)
        assertEquals(0L, items[0].size)
    }

    /** The folder it came from is what the user recognises, not the full path. */
    @Test
    fun `the original folder is the parent of where it was deleted from`() {
        assertEquals(
            "Documents",
            TrashItem(originalLocation = "Documents/notes.md").originalFolder,
        )
    }

    @Test
    fun `an item deleted from the account root reports the root`() {
        assertEquals("Files", TrashItem(originalLocation = "notes.md").originalFolder)
        assertEquals("Files", TrashItem(originalLocation = "").originalFolder)
    }
}
