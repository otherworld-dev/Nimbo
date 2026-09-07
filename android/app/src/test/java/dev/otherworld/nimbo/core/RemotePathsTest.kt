package dev.otherworld.nimbo.core

/*
 * Building the remote paths that rename, move and create-folder send to the
 * server. A rename that computes the wrong target does not fail loudly — it
 * moves the file somewhere else, which is worse.
 */

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class RemotePathsTest {

    // -- joining -------------------------------------------------------------

    @Test
    fun `a child joins onto its parent`() {
        assertEquals("Photos/holiday.jpg", joinRemote("Photos", "holiday.jpg"))
    }

    @Test
    fun `a child at the account root has no leading slash`() {
        assertEquals("holiday.jpg", joinRemote("", "holiday.jpg"))
    }

    @Test
    fun `slashes on either side collapse`() {
        assertEquals("Photos/holiday.jpg", joinRemote("/Photos/", "/holiday.jpg"))
    }

    // -- parents -------------------------------------------------------------

    @Test
    fun `the parent of a nested path drops the last segment`() {
        assertEquals("Photos/Camera", parentOfRemote("Photos/Camera/IMG.jpg"))
    }

    @Test
    fun `the parent of a top-level path is the account root`() {
        assertEquals("", parentOfRemote("Photos"))
    }

    @Test
    fun `the account root has no parent`() {
        assertNull(parentOfRemote(""))
    }

    // -- renaming ------------------------------------------------------------

    @Test
    fun `a rename keeps the file in its folder`() {
        assertEquals(
            "Photos/Camera/beach.jpg",
            renameTargetRemote("Photos/Camera/IMG_2201.jpg", "beach.jpg"),
        )
    }

    @Test
    fun `renaming at the account root stays at the root`() {
        assertEquals("beach.jpg", renameTargetRemote("IMG_2201.jpg", "beach.jpg"))
    }

    /** A name with a separator would silently relocate the file. */
    @Test
    fun `a new name containing a separator is rejected`() {
        assertNull(renameTargetRemote("Photos/IMG.jpg", "../beach.jpg"))
        assertNull(renameTargetRemote("Photos/IMG.jpg", "sub/beach.jpg"))
    }

    @Test
    fun `a blank new name is rejected`() {
        assertNull(renameTargetRemote("Photos/IMG.jpg", "   "))
    }

    // -- name validation -----------------------------------------------------

    @Test
    fun `an ordinary name is accepted`() {
        assertNull(remoteNameProblem("Holiday 2024.jpg"))
        // Nextcloud is happy with a leading dot, unlike the local picker.
        assertNull(remoteNameProblem(".hidden"))
    }

    @Test
    fun `names that would change the path are refused with a reason`() {
        assertEquals("Names can't contain “/”.", remoteNameProblem("a/b"))
        assertEquals("Names can't contain “\\”.", remoteNameProblem("a\\b"))
    }

    @Test
    fun `dot names are refused`() {
        assertEquals("Choose a different name.", remoteNameProblem(".."))
        assertEquals("Choose a different name.", remoteNameProblem("."))
    }

    @Test
    fun `a blank name is refused`() {
        assertEquals("Enter a name.", remoteNameProblem("  "))
    }
}
