package dev.otherworld.nimbo.core

/*
 * The file browser's central claim is that it knows which server files are
 * actually on this phone. That answer comes from mapping a remote path onto a
 * configured sync pair, which is pure string arithmetic — and the place this
 * feature will break if it breaks.
 *
 * The nasty cases, all covered below: a pair rooted at the account root, nested
 * pairs (most specific must win), trailing slashes on either side, and a sibling
 * whose name merely starts with a pair's name ("/Photos2" is NOT inside
 * "/Photos").
 */

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class FilePathsTest {

    private val photos = SyncPair(localDir = "/storage/emulated/0/Nimbo/Photos", remoteRoot = "Photos")
    private val camera = SyncPair(localDir = "/storage/emulated/0/DCIM", remoteRoot = "Photos/Camera")
    private val whole = SyncPair(localDir = "/storage/emulated/0/Nimbo", remoteRoot = "")

    @Test
    fun `a file inside a pair maps onto that pair's local directory`() {
        assertEquals(
            "/storage/emulated/0/Nimbo/Photos/holiday.jpg",
            localPathFor("Photos/holiday.jpg", listOf(photos)),
        )
    }

    @Test
    fun `the pair root itself maps to the local root`() {
        assertEquals("/storage/emulated/0/Nimbo/Photos", localPathFor("Photos", listOf(photos)))
    }

    @Test
    fun `a path outside every pair has no local copy`() {
        assertNull(localPathFor("Documents/tax.pdf", listOf(photos)))
    }

    /** The bug this exists to prevent: prefix matching on the string, not the path. */
    @Test
    fun `a sibling sharing a prefix is not inside the pair`() {
        assertNull(localPathFor("Photos2/holiday.jpg", listOf(photos)))
        assertNull(localPathFor("PhotosArchive", listOf(photos)))
    }

    /** Nested pairs: the most specific root owns the file, not whichever came first. */
    @Test
    fun `the deepest matching pair wins`() {
        val pairs = listOf(photos, camera)
        assertEquals(
            "/storage/emulated/0/DCIM/IMG_2201.jpg",
            localPathFor("Photos/Camera/IMG_2201.jpg", pairs),
        )
        assertEquals(
            "/storage/emulated/0/Nimbo/Photos/holiday.jpg",
            localPathFor("Photos/holiday.jpg", pairs),
        )
    }

    @Test
    fun `the deepest matching pair wins regardless of order`() {
        assertEquals(
            "/storage/emulated/0/DCIM/IMG_2201.jpg",
            localPathFor("Photos/Camera/IMG_2201.jpg", listOf(camera, photos)),
        )
    }

    @Test
    fun `a pair rooted at the account root covers everything`() {
        assertEquals(
            "/storage/emulated/0/Nimbo/Documents/tax.pdf",
            localPathFor("Documents/tax.pdf", listOf(whole)),
        )
        assertEquals("/storage/emulated/0/Nimbo", localPathFor("", listOf(whole)))
    }

    @Test
    fun `leading and trailing slashes on either side are tolerated`() {
        val slashed = SyncPair(localDir = "/storage/emulated/0/Nimbo/Photos/", remoteRoot = "/Photos/")
        assertEquals(
            "/storage/emulated/0/Nimbo/Photos/holiday.jpg",
            localPathFor("/Photos/holiday.jpg/", listOf(slashed)),
        )
    }

    @Test
    fun `no pairs means no local copy`() {
        assertNull(localPathFor("Photos/holiday.jpg", emptyList()))
    }
}
