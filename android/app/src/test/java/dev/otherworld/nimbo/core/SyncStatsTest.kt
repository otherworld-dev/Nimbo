package dev.otherworld.nimbo.core

/*
 * Regression coverage for the "Last sync" card reporting 0 down / 0 up after a
 * successful transfer.
 *
 * The engine fires OnPairSynced for EVERY completed pass, including the no-op
 * poll that immediately follows a real one. The UI stored whatever arrived last,
 * so a genuine "173 up" was overwritten by zeros within seconds — during
 * on-device testing this made a working upload look like a failure. Only a pass
 * that actually moved something may replace the displayed result.
 */

import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class SyncStatsTest {

    @Test
    fun `a pass that moved nothing has no activity`() {
        assertFalse(SyncStats().hasActivity)
    }

    @Test
    fun `an upload counts as activity`() {
        assertTrue(SyncStats(uploaded = 1).hasActivity)
    }

    @Test
    fun `a download counts as activity`() {
        assertTrue(SyncStats(downloaded = 1).hasActivity)
    }

    @Test
    fun `a deletion counts as activity`() {
        assertTrue(SyncStats(delLocal = 1).hasActivity)
        assertTrue(SyncStats(delRemote = 1).hasActivity)
    }

    @Test
    fun `a directory creation counts as activity`() {
        assertTrue(SyncStats(mkLocal = 1).hasActivity)
        assertTrue(SyncStats(mkRemote = 1).hasActivity)
    }

    @Test
    fun `a move counts as activity`() {
        assertTrue(SyncStats(moved = 1).hasActivity)
    }

    @Test
    fun `a conflict counts as activity`() {
        assertTrue(SyncStats(conflicts = 1).hasActivity)
        assertTrue(SyncStats(conflictsIdentical = 1).hasActivity)
        assertTrue(SyncStats(conflictsResurrected = 1).hasActivity)
    }

    /** A failed pass is the most important one to keep on screen. */
    @Test
    fun `a failure counts as activity`() {
        assertTrue(SyncStats(failed = 1).hasActivity)
    }
}
