package dev.otherworld.nimbo.supporter

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class KeyRulesTest {

    private val good = "owr_live_" + "abcdefghjk".repeat(2)

    @Test
    fun `only keys shaped like Otherworld's are accepted`() {
        assertTrue(isKeyShaped(good))
        assertTrue(isKeyShaped("  $good \n"))
        assertFalse(isKeyShaped("owr_live_short"))
        assertFalse(isKeyShaped("owr_test_" + "a".repeat(20)))
        // 'i', 'l', 'o', 'u', '0', '1' are not in the alphabet.
        assertFalse(isKeyShaped("owr_live_" + "i".repeat(20)))
        assertFalse(isKeyShaped(""))
    }

    @Test
    fun `a masked key can be recognised but not used`() {
        assertEquals("owr_live_abcd…", maskKey(good))
    }

    @Test
    fun `the add link yields its key, and nothing else does`() {
        assertEquals(good, keyFromLink("nimbo-supporter://add?key=$good"))
        assertEquals(good, keyFromLink("nimbo-supporter://add?from=web&key=$good"))
        assertNull(keyFromLink("nimbo-supporter://remove?key=$good"))
        assertNull(keyFromLink("https://add?key=$good"))
        assertNull(keyFromLink("nimbo-supporter://add"))
        assertNull(keyFromLink("nimbo-supporter://add?key=owr_live_nope"))
        assertNull(keyFromLink("not a uri at all %%"))
    }

    @Test
    fun `server tiers map to app tiers, and a new one is at least a supporter`() {
        assertEquals(SupporterTier.ONE_OFF, tierFromServer("nimbo_tip"))
        assertEquals(SupporterTier.SUPPORTER, tierFromServer("nimbo_supporter"))
        assertEquals(SupporterTier.BACKER, tierFromServer("nimbo_backer"))
        assertEquals(SupporterTier.PATRON, tierFromServer("nimbo_patron"))
        assertEquals(SupporterTier.SUPPORTER, tierFromServer("nimbo_champion"))
    }

    @Test
    fun `only definite answers are outcomes`() {
        assertEquals(CheckOutcome.Active(SupporterTier.PATRON), outcomeOf(200, """{"tier":"nimbo_patron","status":"active"}"""))
        assertEquals(CheckOutcome.Ended, outcomeOf(401, """{"error":"Unknown key."}"""))
        assertEquals(CheckOutcome.Paused, outcomeOf(403, """{"reason":"paused"}"""))
        assertEquals(CheckOutcome.Ended, outcomeOf(403, """{"reason":"revoked"}"""))
        assertEquals(CheckOutcome.Ended, outcomeOf(403, """{"reason":"not_entitled"}"""))
        // Anything we can't read is no answer, never a downgrade.
        assertEquals(CheckOutcome.Unreachable, outcomeOf(403, "<html>forbidden</html>"))
        assertEquals(CheckOutcome.Unreachable, outcomeOf(200, "<html>captive portal</html>"))
        assertEquals(CheckOutcome.Unreachable, outcomeOf(500, null))
        assertEquals(CheckOutcome.Unreachable, outcomeOf(502, "Bad gateway"))
        assertEquals(CheckOutcome.Unreachable, outcomeOf(404, null))
    }

    @Test
    fun `a key's state follows its outcome, and no answer changes nothing`() {
        val key = StoredKey(good, SupporterTier.BACKER, KeyState.ACTIVE)
        assertEquals(key, key.after(CheckOutcome.Unreachable))
        assertEquals(KeyState.PAUSED, key.after(CheckOutcome.Paused).state)
        assertEquals(SupporterTier.BACKER, key.after(CheckOutcome.Paused).tier)
        assertEquals(KeyState.ENDED, key.after(CheckOutcome.Ended).state)
        assertEquals(StoredKey(good, SupporterTier.PATRON, KeyState.ACTIVE), key.after(CheckOutcome.Active(SupporterTier.PATRON)))
    }

    @Test
    fun `paused keys still count, ended ones don't`() {
        val keys = listOf(
            StoredKey("a", SupporterTier.PATRON, KeyState.ENDED),
            StoredKey("b", SupporterTier.BACKER, KeyState.PAUSED),
            StoredKey("c", SupporterTier.ONE_OFF, KeyState.ACTIVE),
        )
        assertEquals(SupporterTier.BACKER, tierOf(keys))
    }

    @Test
    fun `a tip is never sent to the billing portal`() {
        val tip = StoredKey("t", SupporterTier.ONE_OFF, KeyState.ACTIVE)
        val sub = StoredKey("s", SupporterTier.SUPPORTER, KeyState.ACTIVE)
        assertNull(manageableKey(listOf(tip)))
        assertEquals(sub, manageableKey(listOf(tip, sub)))
    }

    @Test
    fun `a key whose first check was paused can still open the portal`() {
        val paused = StoredKey("p", SupporterTier.NONE, KeyState.PAUSED)
        assertEquals(paused, manageableKey(listOf(paused)))
    }

    @Test
    fun `a subscriber changes tier in the portal, never with a second checkout`() {
        val sub = listOf(StoredKey("s", SupporterTier.SUPPORTER, KeyState.ACTIVE))
        assertEquals(MonthlyAction.CHECKOUT, monthlyActionFor(emptyList(), SupporterTier.BACKER))
        assertEquals(MonthlyAction.CURRENT, monthlyActionFor(sub, SupporterTier.SUPPORTER))
        assertEquals(MonthlyAction.PORTAL, monthlyActionFor(sub, SupporterTier.PATRON))
    }

    @Test
    fun `checks are repeated after three days`() {
        val day = 24L * 60 * 60 * 1000
        assertFalse(dueForCheck(now = 10 * day, lastCheckedAt = 8 * day))
        assertTrue(dueForCheck(now = 10 * day, lastCheckedAt = 7 * day))
    }
}
