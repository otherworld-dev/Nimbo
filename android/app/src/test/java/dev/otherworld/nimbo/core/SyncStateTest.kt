package dev.otherworld.nimbo.core

/*
 * Classifying a server file for the browser: is it on this phone, is it only on
 * the server, is it still coming, is it in trouble?
 *
 * Precedence matters and is asserted explicitly. A conflicted file is conflicted
 * even though it exists locally — that is the whole point of surfacing it — and
 * a file mid-transfer reads as pending even if a stale copy is already on disk.
 */

import org.junit.Assert.assertEquals
import org.junit.Test

class SyncStateTest {

    private val photos = SyncPair(localDir = "/sd/Nimbo/Photos", remoteRoot = "Photos")
    private val pairs = listOf(photos)

    private fun state(
        path: String,
        onDisk: Set<String> = emptySet(),
        conflicted: Set<String> = emptySet(),
        inFlight: Set<String> = emptySet(),
    ) = syncStateFor(
        remotePath = path,
        pairs = pairs,
        existsLocally = { it in onDisk },
        conflictedPaths = conflicted,
        inFlightPaths = inFlight,
    )

    @Test
    fun `a file outside every pair is server-only`() {
        assertEquals(SyncState.SERVER_ONLY, state("Documents/tax.pdf"))
    }

    @Test
    fun `a file inside a pair and present on disk is synced`() {
        assertEquals(
            SyncState.SYNCED,
            state("Photos/holiday.jpg", onDisk = setOf("/sd/Nimbo/Photos/holiday.jpg")),
        )
    }

    @Test
    fun `a file inside a pair but not yet on disk is pending`() {
        assertEquals(SyncState.PENDING, state("Photos/holiday.jpg"))
    }

    @Test
    fun `a file being transferred reads as pending even if a copy is on disk`() {
        assertEquals(
            SyncState.PENDING,
            state(
                "Photos/holiday.jpg",
                onDisk = setOf("/sd/Nimbo/Photos/holiday.jpg"),
                inFlight = setOf("Photos/holiday.jpg"),
            ),
        )
    }

    /** Conflict outranks everything: it is the state the user must act on. */
    @Test
    fun `a conflicted file is conflicted even when it is on disk`() {
        assertEquals(
            SyncState.CONFLICTED,
            state(
                "Photos/holiday.jpg",
                onDisk = setOf("/sd/Nimbo/Photos/holiday.jpg"),
                conflicted = setOf("Photos/holiday.jpg"),
            ),
        )
    }

    @Test
    fun `conflict outranks server-only`() {
        assertEquals(
            SyncState.CONFLICTED,
            state("Documents/tax.pdf", conflicted = setOf("Documents/tax.pdf")),
        )
    }

    @Test
    fun `slashes do not defeat the conflict and in-flight lookups`() {
        assertEquals(
            SyncState.CONFLICTED,
            state("Photos/holiday.jpg", conflicted = setOf("/Photos/holiday.jpg/")),
        )
    }
}
