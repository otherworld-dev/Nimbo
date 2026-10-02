package dev.otherworld.nimbo.supporter

import org.junit.Assert.assertEquals
import org.junit.Test

class NudgeTest {

    private val day = 24L * 60 * 60 * 1000
    private val now = 100L * day

    private fun show(
        firstSeenAt: Long = now - 15 * day,
        lastSyncAt: Long = now - day / 2,
        failing: Int = 0,
        frozen: Int = 0,
        tier: SupporterTier = SupporterTier.NONE,
        retired: Boolean = false,
    ) = shouldShowNudge(now, firstSeenAt, lastSyncAt, failing, frozen, tier, retired)

    @Test
    fun `each condition on its own can hold the card back`() {
        val cases = listOf(
            "all good" to (show() to true),
            "only 13 days in" to (show(firstSeenAt = now - 13 * day) to false),
            "never seen Home" to (show(firstSeenAt = 0) to false),
            "no sync for two days" to (show(lastSyncAt = now - 2 * day) to false),
            "never synced" to (show(lastSyncAt = 0) to false),
            "a folder failing" to (show(failing = 1) to false),
            "a folder frozen" to (show(frozen = 1) to false),
            "already a supporter" to (show(tier = SupporterTier.ONE_OFF) to false),
            "dismissed before" to (show(retired = true) to false),
            "exactly 14 days" to (show(firstSeenAt = now - 14 * day) to true),
        )
        for ((name, case) in cases) assertEquals(name, case.second, case.first)
    }
}
