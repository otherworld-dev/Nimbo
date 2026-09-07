package dev.otherworld.nimbo.ui.screens

/*
 * The "Last sync" card used to render exactly two chips — "N down" and "N up" —
 * so any pass whose work was not a download or an upload reported "0 down, 0 up".
 * Observed on device: deleting a file synced the deletion to the server and the
 * card still read 0/0, which is indistinguishable from "nothing happened".
 *
 * Every non-zero counter must produce a chip.
 */

import dev.otherworld.nimbo.core.SyncStats
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

class SyncStatLabelsTest {

    private fun texts(stats: SyncStats) = syncStatLabels(stats).map { it.text }

    @Test
    fun `a remote deletion is reported`() {
        assertEquals(listOf("1 deleted on server"), texts(SyncStats(delRemote = 1)))
    }

    @Test
    fun `a local deletion is reported`() {
        assertEquals(listOf("2 deleted here"), texts(SyncStats(delLocal = 2)))
    }

    @Test
    fun `downloads and uploads are reported together`() {
        assertEquals(
            listOf("3 downloaded", "4 uploaded"),
            texts(SyncStats(downloaded = 3, uploaded = 4)),
        )
    }

    @Test
    fun `a move is reported`() {
        assertEquals(listOf("1 moved"), texts(SyncStats(moved = 1)))
    }

    @Test
    fun `new folders are reported for each side`() {
        assertEquals(
            listOf("1 folder here", "2 folders on server"),
            texts(SyncStats(mkLocal = 1, mkRemote = 2)),
        )
    }

    @Test
    fun `zero counters produce no chip`() {
        assertTrue(texts(SyncStats(uploaded = 1)).none { it.contains("download") })
    }

    @Test
    fun `conflicts and failures are flagged as problems`() {
        val conflict = syncStatLabels(SyncStats(conflicts = 1)).single()
        assertEquals("1 conflict", conflict.text)
        assertTrue(conflict.isProblem)

        val failed = syncStatLabels(SyncStats(failed = 2)).single()
        assertEquals("2 failed", failed.text)
        assertTrue(failed.isProblem)
    }

    @Test
    fun `counts are pluralised`() {
        assertEquals(listOf("1 conflict"), texts(SyncStats(conflicts = 1)))
        assertEquals(listOf("2 conflicts"), texts(SyncStats(conflicts = 2)))
    }

    /** A pass with nothing to report still needs something on screen. */
    @Test
    fun `an empty pass says so rather than showing zeros`() {
        assertEquals(listOf("Nothing to do"), texts(SyncStats()))
    }
}
