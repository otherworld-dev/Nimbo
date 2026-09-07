/*
 * NotificationPlan.kt — deciding what the Android shade should show.
 *
 * Pure, so the judgement can be tested directly rather than inferred from
 * watching a phone. ServerNotifications does the talking to Android; this only
 * decides what to say.
 */
package dev.otherworld.nimbo.service

import dev.otherworld.nimbo.core.NcNotification

/**
 * The outcome of comparing the server's current notifications against what was
 * last shown.
 *
 * [toRemember] is always the current set, including when nothing is posted —
 * recording it is what stops the next pass looking like a first run.
 */
data class NotificationPlan(
    val toPost: List<NcNotification>,
    val toCancel: Set<Int>,
    val toRemember: Set<Int>,
) {
    companion object {
        /**
         * Works out what changed.
         *
         * [seen] is null only on the very first pass for an account, and that
         * pass posts nothing: everything on the server has been sitting there
         * unread, often for weeks, and emptying a backlog into someone's shade
         * the moment they sign in is how an app gets silenced for good. It
         * still records what it found, so the next real arrival is noticed.
         */
        fun of(seen: Set<Int>?, current: List<NcNotification>): NotificationPlan {
            val ids = current.map { it.id }.toSet()
            if (seen == null) {
                return NotificationPlan(emptyList(), emptySet(), ids)
            }
            return NotificationPlan(
                toPost = current.filter { it.id !in seen },
                // Gone from the server means gone from the shade, whether it was
                // dismissed here, in the web UI, or on another device.
                toCancel = seen - ids,
                toRemember = ids,
            )
        }
    }
}
