package dev.otherworld.nimbo.supporter

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class SupporterModelTest {

    @Test
    fun `nobody gets perks for free`() {
        assertEquals(Perks(badge = false, icons = false, accents = false), Perks.of(SupporterTier.NONE))
    }

    @Test
    fun `a one-off gets the badge and nothing else`() {
        assertEquals(Perks(badge = true, icons = false, accents = false), Perks.of(SupporterTier.ONE_OFF))
    }

    @Test
    fun `each monthly tier adds one perk`() {
        assertEquals(Perks(badge = true, icons = false, accents = false), Perks.of(SupporterTier.SUPPORTER))
        assertEquals(Perks(badge = true, icons = true, accents = false), Perks.of(SupporterTier.BACKER))
        assertEquals(Perks(badge = true, icons = true, accents = true), Perks.of(SupporterTier.PATRON))
    }

    @Test
    fun `the higher of a tip and a membership wins`() {
        assertEquals(SupporterTier.BACKER, highestTier(listOf(SupporterTier.ONE_OFF, SupporterTier.BACKER)))
        assertEquals(SupporterTier.NONE, highestTier(emptyList()))
    }

    @Test
    fun `a status survives the cache`() {
        val status = SupporterStatus(SupporterTier.PATRON, SupporterSource.KEY, 1_700_000_000_000)
        assertEquals(status, SupporterStatus.decode(status.encode()))
    }

    @Test
    fun `an unreadable cache is not a supporter, never a crash`() {
        for (raw in listOf(null, "", "PATRON", "GOLD|KEY|1", "PATRON|MAIL|1", "PATRON|KEY|soon", "a|b|c|d")) {
            assertEquals(raw.toString(), SupporterStatus(), SupporterStatus.decode(raw))
        }
    }

    @Test
    fun `a debug override replaces the real tier, the real status is kept`() {
        val real = SupporterStatus(SupporterTier.NONE, SupporterSource.NONE, 0)
        val ui = supporterUi(real, null, SupporterChoices(debugOverride = SupporterTier.PATRON))
        assertEquals(SupporterTier.PATRON, ui.tier)
        assertTrue(ui.perks.accents)
        assertEquals(SupporterTier.NONE, ui.status.tier)
    }

    @Test
    fun `perk lines name what each tier adds`() {
        assertTrue(perkLines(SupporterTier.BACKER).contains("Alternative app icons"))
        assertTrue(perkLines(SupporterTier.PATRON).contains("Accent colour themes"))
        assertFalse(perkLines(SupporterTier.SUPPORTER).contains("Alternative app icons"))
    }
}
