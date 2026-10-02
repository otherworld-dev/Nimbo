package dev.otherworld.nimbo.supporter

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class PlayRulesTest {

    private fun owned(id: String, purchased: Boolean = true, pending: Boolean = false, acknowledged: Boolean = true) =
        OwnedProduct(id, "token-$id", purchased, pending, acknowledged)

    @Test
    fun `products map to tiers`() {
        assertEquals(SupporterTier.SUPPORTER, tierForProduct("supporter_monthly"))
        assertEquals(SupporterTier.BACKER, tierForProduct("backer_monthly"))
        assertEquals(SupporterTier.PATRON, tierForProduct("patron_monthly"))
        for (tip in listOf("tip_small", "tip_medium", "tip_large")) assertEquals(SupporterTier.ONE_OFF, tierForProduct(tip))
        assertEquals(SupporterTier.NONE, tierForProduct("something_else"))
    }

    @Test
    fun `the highest purchase wins, and pending ones unlock nothing`() {
        assertEquals(SupporterTier.BACKER, playTier(listOf(owned("tip_small"), owned("backer_monthly"))))
        assertEquals(SupporterTier.NONE, playTier(listOf(owned("patron_monthly", purchased = false, pending = true))))
        assertTrue(anyPending(listOf(owned("tip_large", purchased = false, pending = true))))
    }

    @Test
    fun `every unacknowledged purchase is acknowledged, pending ones are not`() {
        val list = listOf(
            owned("tip_small", acknowledged = false),
            owned("tip_medium"),
            owned("tip_large", purchased = false, pending = true, acknowledged = false),
        )
        assertEquals(listOf("tip_small"), needsAcknowledging(list).map { it.productId })
    }

    @Test
    fun `changing tier upgrades now and downgrades at renewal`() {
        val backer = owned("backer_monthly")
        assertEquals(Change.NEW, changeFor(null, "supporter_monthly"))
        assertEquals(Change.SAME, changeFor(backer, "backer_monthly"))
        assertEquals(Change.UPGRADE, changeFor(backer, "patron_monthly"))
        assertEquals(Change.DOWNGRADE, changeFor(backer, "supporter_monthly"))
    }

    @Test
    fun `the current subscription is the highest one, never a tip`() {
        assertNull(currentSubscription(listOf(owned("tip_large"))))
        assertEquals("patron_monthly", currentSubscription(listOf(owned("supporter_monthly"), owned("patron_monthly")))!!.productId)
    }

    @Test
    fun `manage opens Play's subscriptions page for this app`() {
        assertEquals(
            "https://play.google.com/store/account/subscriptions?package=dev.otherworld.nimbo&sku=backer_monthly",
            manageUrl("backer_monthly"),
        )
    }

    @Test
    fun `a phone without Play says so instead of offering buttons that do nothing`() {
        assertEquals("Google Play purchases aren't available on this device.", unavailableMessage(billingUnavailable = true))
        assertEquals("Couldn't reach Google Play. Try again.", unavailableMessage(billingUnavailable = false))
        assertFalse(unavailableMessage(true).isBlank())
    }
}
