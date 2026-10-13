package dev.otherworld.nimbo.supporter

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class IconSwitchRulesTest {

    private val nimbo = "dev.otherworld.nimbo"

    @Test
    fun `the icon only changes when nobody is in Nimbo`() {
        val cases = listOf(
            "left for Home, or Back out of the app" to (canSwitchIcon(0, screenWentOff = false, otherAppOnTop = false) to true),
            "a screen is showing" to (canSwitchIcon(1, screenWentOff = false, otherAppOnTop = false) to false),
            "two screens showing" to (canSwitchIcon(2, screenWentOff = false, otherAppOnTop = false) to false),
            "the phone was locked on Nimbo" to (canSwitchIcon(0, screenWentOff = true, otherAppOnTop = false) to false),
            "the file picker covers Nimbo" to (canSwitchIcon(0, screenWentOff = false, otherAppOnTop = true) to false),
            "locked with the picker open" to (canSwitchIcon(0, screenWentOff = true, otherAppOnTop = true) to false),
        )
        for ((name, case) in cases) assertEquals(name, case.second, case.first)
    }

    @Test
    fun `another app's screen on top of a Nimbo task counts as still in Nimbo`() {
        assertTrue(hasOtherAppOnTop(nimbo, listOf("com.android.documentsui")))
        assertTrue(hasOtherAppOnTop(nimbo, listOf(nimbo, "com.android.chrome")))
        // Android can hand back a blank package for a screen that isn't ours.
        assertTrue(hasOtherAppOnTop(nimbo, listOf("")))
    }

    @Test
    fun `Nimbo's own screen on top, or no screens at all, is not another app`() {
        assertFalse(hasOtherAppOnTop(nimbo, listOf(nimbo)))
        // Back finished the only screen: the task is left with nothing on top.
        assertFalse(hasOtherAppOnTop(nimbo, listOf(null)))
        // Swiped away from recents: no task.
        assertFalse(hasOtherAppOnTop(nimbo, emptyList()))
    }
}
