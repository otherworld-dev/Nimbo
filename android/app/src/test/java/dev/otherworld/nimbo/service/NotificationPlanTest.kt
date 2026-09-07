package dev.otherworld.nimbo.service

import dev.otherworld.nimbo.core.NcNotification
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * What reaches the Android shade, and what is taken back down.
 *
 * This is the whole of the mirror's judgement, so it is a pure function tested
 * directly rather than something only observable by watching a phone.
 */
class NotificationPlanTest {

    private fun notif(id: Int) = NcNotification(id = id, subject = "n$id")

    @Test
    fun `the first run announces nothing`() {
        // Everything on the server has been sitting unread, often for weeks.
        // Posting it all the moment someone signs in is how an app gets its
        // notifications switched off for good.
        val plan = NotificationPlan.of(seen = null, current = listOf(notif(1), notif(2), notif(3)))
        assertTrue(plan.toPost.isEmpty())
        assertTrue(plan.toCancel.isEmpty())
        assertEquals(setOf(1, 2, 3), plan.toRemember)
    }

    @Test
    fun `only genuinely new notifications are posted`() {
        val plan = NotificationPlan.of(seen = setOf(1, 2), current = listOf(notif(1), notif(2), notif(3)))
        assertEquals(listOf(3), plan.toPost.map { it.id })
        assertTrue(plan.toCancel.isEmpty())
    }

    @Test
    fun `anything gone from the server is taken out of the shade`() {
        // Dismissed in the web UI or on another phone: it should not linger here.
        val plan = NotificationPlan.of(seen = setOf(1, 2, 3), current = listOf(notif(2)))
        assertTrue(plan.toPost.isEmpty())
        assertEquals(setOf(1, 3), plan.toCancel)
        assertEquals(setOf(2), plan.toRemember)
    }

    @Test
    fun `arrivals and departures in the same pass are both handled`() {
        val plan = NotificationPlan.of(seen = setOf(1, 2), current = listOf(notif(2), notif(9)))
        assertEquals(listOf(9), plan.toPost.map { it.id })
        assertEquals(setOf(1), plan.toCancel)
        assertEquals(setOf(2, 9), plan.toRemember)
    }

    @Test
    fun `an unchanged list does nothing at all`() {
        // The count callback fires on any change, including ones that do not
        // alter the set; a repeat pass must not re-post what is already shown.
        val plan = NotificationPlan.of(seen = setOf(1, 2), current = listOf(notif(1), notif(2)))
        assertTrue(plan.toPost.isEmpty())
        assertTrue(plan.toCancel.isEmpty())
    }

    @Test
    fun `an emptied server clears the shade but is not treated as a first run`() {
        val plan = NotificationPlan.of(seen = setOf(1, 2), current = emptyList())
        assertEquals(setOf(1, 2), plan.toCancel)
        assertTrue(plan.toRemember.isEmpty())
    }

    @Test
    fun `a first run with nothing on the server still records that it ran`() {
        // Otherwise the next pass looks like another first run and the account's
        // first real notification is swallowed.
        val plan = NotificationPlan.of(seen = null, current = emptyList())
        assertTrue(plan.toPost.isEmpty())
        assertTrue(plan.toRemember.isEmpty())
    }
}
