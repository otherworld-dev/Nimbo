package dev.otherworld.nimbo.platform

/*
 * The local folder picker described a folder holding 60 files as "Empty",
 * because the subtitle counted subfolders only. Seen on device while choosing a
 * sync destination — exactly the moment the user needs to know whether a folder
 * has anything in it.
 */

import org.junit.Assert.assertEquals
import org.junit.Test

class LocalFsLabelTest {

    @Test
    fun `a truly empty folder says so`() {
        assertEquals("Empty", folderSubtitle(dirs = 0, files = 0))
    }

    /** The bug: files present, no subfolders, previously reported as empty. */
    @Test
    fun `files alone are counted`() {
        assertEquals("60 files", folderSubtitle(dirs = 0, files = 60))
    }

    @Test
    fun `folders alone are counted`() {
        assertEquals("2 folders", folderSubtitle(dirs = 2, files = 0))
    }

    @Test
    fun `both are shown together`() {
        assertEquals("2 folders · 3 files", folderSubtitle(dirs = 2, files = 3))
    }

    @Test
    fun `singulars read naturally`() {
        assertEquals("1 folder", folderSubtitle(dirs = 1, files = 0))
        assertEquals("1 file", folderSubtitle(dirs = 0, files = 1))
        assertEquals("1 folder · 1 file", folderSubtitle(dirs = 1, files = 1))
    }

    /** Unreadable folders report zero rather than failing; do not claim "Empty". */
    @Test
    fun `an unknown count is not described as empty`() {
        assertEquals("", folderSubtitle(dirs = -1, files = -1))
    }
}
