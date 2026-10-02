package dev.otherworld.nimbo.supporter

import dev.otherworld.nimbo.ui.theme.readableOnArgb
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import kotlin.math.pow

class CosmeticsTest {

    @Test
    fun `a lapsed supporter sees the defaults but keeps their choices`() {
        val lapsed = Perks.of(SupporterTier.NONE)
        assertEquals(AppIcon.DEFAULT, effectiveIcon(AppIcon.EMBER, lapsed))
        assertEquals(AccentChoice.FOLLOW_NEXTCLOUD, effectiveAccent(AccentChoice.TEAL, lapsed))

        val ui = supporterUi(SupporterStatus(), null, SupporterChoices(icon = AppIcon.EMBER, accent = AccentChoice.TEAL))
        assertEquals(AppIcon.EMBER, ui.icon)
        assertEquals(AppIcon.DEFAULT, ui.effectiveIcon)
        assertEquals(AccentChoice.TEAL, ui.accent)
        assertEquals(AccentChoice.FOLLOW_NEXTCLOUD, ui.effectiveAccent)
    }

    @Test
    fun `resubscribing brings the choices straight back`() {
        assertEquals(AppIcon.EMBER, effectiveIcon(AppIcon.EMBER, Perks.of(SupporterTier.BACKER)))
        assertEquals(AccentChoice.TEAL, effectiveAccent(AccentChoice.TEAL, Perks.of(SupporterTier.PATRON)))
        // A Backer has icons but not accents.
        assertEquals(AccentChoice.FOLLOW_NEXTCLOUD, effectiveAccent(AccentChoice.TEAL, Perks.of(SupporterTier.BACKER)))
    }

    @Test
    fun `stored names that no longer exist fall back to the defaults`() {
        assertEquals(AppIcon.DEFAULT, AppIcon.fromStored("RAINBOW"))
        assertEquals(AppIcon.DEFAULT, AppIcon.fromStored(null))
        assertEquals(AccentChoice.FOLLOW_NEXTCLOUD, AccentChoice.fromStored("GOLD"))
    }

    // WCAG relative luminance — the yardstick the fixed accents are held to.
    private fun channel(argb: Long, shift: Int): Double {
        val c = ((argb shr shift) and 0xFF) / 255.0
        return if (c <= 0.04045) c / 12.92 else ((c + 0.055) / 1.055).pow(2.4)
    }

    private fun luminance(argb: Long) =
        0.2126 * channel(argb, 16) + 0.7152 * channel(argb, 8) + 0.0722 * channel(argb, 0)

    private fun contrast(a: Long, b: Long): Double {
        val (hi, lo) = listOf(luminance(a), luminance(b)).sortedDescending()
        return (hi + 0.05) / (lo + 0.05)
    }

    @Test
    fun `every fixed accent is readable under its own text colour`() {
        for (accent in AccentChoice.entries) {
            val argb = accent.argb ?: continue
            val ratio = contrast(argb, readableOnArgb(argb))
            assertTrue("${accent.label}: $ratio", ratio >= 4.5)
        }
    }

    @Test
    fun `every fixed accent stands out on both the light and the dark theme`() {
        val lightSurface = 0xFFFCFCFF
        val darkSurface = 0xFF1A1C1E
        for (accent in AccentChoice.entries) {
            val argb = accent.argb ?: continue
            assertTrue("${accent.label} on light", contrast(argb, lightSurface) >= 3.0)
            assertTrue("${accent.label} on dark", contrast(argb, darkSurface) >= 3.0)
        }
    }
}
