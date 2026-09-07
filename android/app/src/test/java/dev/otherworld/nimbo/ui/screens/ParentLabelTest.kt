package dev.otherworld.nimbo.ui.screens

import org.junit.Assert.assertEquals
import org.junit.Test

/**
 * A favourite can come from anywhere in the account, so its row has to say
 * where. These are the cases where "the folder it's in" is not just
 * substringBeforeLast('/').
 */
class ParentLabelTest {

    @Test
    fun `a top-level item reports the account root as Nextcloud names it`() {
        assertEquals("Files", parentLabel("report.pdf"))
        assertEquals("Files", parentLabel("/report.pdf"))
    }

    @Test
    fun `a nested item reports its full parent path, not just the leaf`() {
        // "Work" alone would be ambiguous across Documents/Work and Archive/Work.
        assertEquals("Documents/Work", parentLabel("Documents/Work/report.pdf"))
    }

    @Test
    fun `a folder is labelled by its parent, not by itself`() {
        assertEquals("Documents", parentLabel("Documents/Work/"))
    }

    @Test
    fun `an empty path does not crash or invent a folder`() {
        assertEquals("Files", parentLabel(""))
        assertEquals("Files", parentLabel("/"))
        assertEquals("Files", parentLabel("   "))
    }
}
