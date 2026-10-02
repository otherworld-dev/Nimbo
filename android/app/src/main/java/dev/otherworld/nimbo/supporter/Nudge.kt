/*
 * Nudge.kt — the one-time "support Nimbo" card on the Sync tab.
 *
 * Shown once, ever, and only when Nimbo has plainly earned it: two weeks in,
 * syncing fine right now, nothing broken. Asking while a folder is failing
 * would be asking at the worst possible moment.
 */
package dev.otherworld.nimbo.supporter

const val NUDGE_AFTER_MS = 14L * 24 * 60 * 60 * 1000
const val NUDGE_RECENT_SYNC_MS = 24L * 60 * 60 * 1000

fun shouldShowNudge(
    now: Long,
    firstSeenAt: Long,
    lastSyncAt: Long,
    failingCount: Int,
    frozenCount: Int,
    tier: SupporterTier,
    retired: Boolean,
): Boolean =
    !retired &&
        tier == SupporterTier.NONE &&
        firstSeenAt > 0 && now - firstSeenAt >= NUDGE_AFTER_MS &&
        lastSyncAt > 0 && now - lastSyncAt <= NUDGE_RECENT_SYNC_MS &&
        failingCount == 0 && frozenCount == 0
