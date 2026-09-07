package dev.otherworld.nimbo.ui.theme

import androidx.compose.ui.graphics.Color
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * The theme's three judgements: what colour the server asked for, what to write
 * on top of it, and whether we are in dark mode.
 *
 * All three are decided from values a server admin or a user controls, so all
 * three have to cope with values neither of them thought about.
 */
class ThemingTest {

    // ---- the server's colour ------------------------------------------

    @Test
    fun `parses the hex a Nextcloud server reports`() {
        assertEquals(Color(0xFF0082C9), parseThemeColor("#0082C9"))
        // Nextcloud reports lowercase in some versions.
        assertEquals(Color(0xFF0082C9), parseThemeColor("#0082c9"))
    }

    @Test
    fun `tolerates a missing hash and surrounding space`() {
        assertEquals(Color(0xFF0082C9), parseThemeColor("0082C9"))
        assertEquals(Color(0xFF0082C9), parseThemeColor("  #0082C9  "))
    }

    @Test
    fun `accepts the three-digit shorthand`() {
        // #FA0 is #FFAA00 — a server admin can type either into Nextcloud.
        assertEquals(Color(0xFFFFAA00), parseThemeColor("#FA0"))
    }

    @Test
    fun `refuses anything it cannot read rather than guessing a colour`() {
        // A wrong colour is worse than the app's own: it could be unreadable.
        for (bad in listOf("", "   ", "#", "#12", "#12345", "nope", "#GGGGGG", "#0082C9FF00")) {
            assertNull("expected null for ${'"'}$bad${'"'}", parseThemeColor(bad))
        }
    }

    // ---- what goes on top of it ---------------------------------------

    @Test
    fun `writes black on light accents and white on dark ones`() {
        // A server admin can pick any colour; text on it still has to be read.
        assertEquals(Color.White, readableOn(Color(0xFF0082C9)))   // Nextcloud blue
        assertEquals(Color.White, readableOn(Color(0xFF000000)))
        assertEquals(Color.Black, readableOn(Color(0xFFFFFFFF)))
        assertEquals(Color.Black, readableOn(Color(0xFFFFE066)))   // pale yellow
    }

    @Test
    fun `judges by luminance, not by brightness of one channel`() {
        // Pure green is far lighter to the eye than pure blue, despite both
        // being one full channel. Naive averaging gets this wrong.
        assertEquals(Color.Black, readableOn(Color(0xFF00FF00)))
        assertEquals(Color.White, readableOn(Color(0xFF0000FF)))
    }

    // ---- dark or light ------------------------------------------------

    @Test
    fun `an explicit choice wins over everything else`() {
        for (server in listOf("dark", "light", "default", "")) {
            assertTrue(resolveDark(AppearancePreference.ALWAYS_DARK, server, systemDark = false))
            assertTrue(!resolveDark(AppearancePreference.ALWAYS_LIGHT, server, systemDark = true))
        }
    }

    @Test
    fun `follow phone ignores the server entirely`() {
        assertTrue(resolveDark(AppearancePreference.FOLLOW_SYSTEM, "light", systemDark = true))
        assertTrue(!resolveDark(AppearancePreference.FOLLOW_SYSTEM, "dark", systemDark = false))
    }

    @Test
    fun `follow Nextcloud honours an explicit server appearance`() {
        assertTrue(resolveDark(AppearancePreference.FOLLOW_NEXTCLOUD, "dark", systemDark = false))
        assertTrue(!resolveDark(AppearancePreference.FOLLOW_NEXTCLOUD, "light", systemDark = true))
    }

    @Test
    fun `follow Nextcloud falls back to the phone when the server has no opinion`() {
        // "default" means the user told Nextcloud to follow THEIR system, so
        // there is nothing to follow but ours. Unknown and empty are the same
        // case: we were not told.
        for (server in listOf("default", "", "   ", "something-new")) {
            assertTrue(resolveDark(AppearancePreference.FOLLOW_NEXTCLOUD, server, systemDark = true))
            assertTrue(!resolveDark(AppearancePreference.FOLLOW_NEXTCLOUD, server, systemDark = false))
        }
    }

    @Test
    fun `server appearance is matched case-insensitively`() {
        assertTrue(resolveDark(AppearancePreference.FOLLOW_NEXTCLOUD, "Dark", systemDark = false))
    }

    // ---- persistence round trip ---------------------------------------

    @Test
    fun `a stored preference survives, and an unknown one does not crash`() {
        for (p in AppearancePreference.entries) {
            assertEquals(p, AppearancePreference.fromStored(p.stored))
        }
        // A value written by a future version, or a corrupted one.
        assertEquals(AppearancePreference.FOLLOW_NEXTCLOUD, AppearancePreference.fromStored("wat"))
        assertEquals(AppearancePreference.FOLLOW_NEXTCLOUD, AppearancePreference.fromStored(null))
    }
}
