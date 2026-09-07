/*
 * ServerNotifications.kt — mirrors the server's notification list into the
 * Android shade.
 *
 * The engine tells us only that the set changed and how many there are, so the
 * list is re-fetched and diffed against what we last saw. That diff does the
 * work in both directions: what appeared gets posted, what vanished gets taken
 * back down — including things dismissed from the web UI or another phone.
 */
package dev.otherworld.nimbo.service

import android.content.Context
import android.util.Log
import androidx.core.content.edit
import dev.otherworld.nimbo.core.NimboCore

object ServerNotifications {

    private const val TAG = "NimboServerNotifs"
    private const val PREFS = "server_notifications"
    private const val KEY_SEEN = "seen_ids"

    /**
     * Re-reads the server's notifications and reconciles the shade with them.
     *
     * The first run after signing in posts NOTHING. Everything already on the
     * server has been sitting there unread — often for weeks — and dumping
     * nineteen notifications into someone's shade the moment they sign in is
     * how an app gets its notifications turned off permanently. That first pass
     * records what exists; only what arrives afterwards is announced.
     */
    suspend fun sync(context: Context) {
        // The list is a cache the engine only refills on a push event; ask for a
        // fresh one first, or a silent push channel means this never sees
        // anything new. A failed refresh still falls through to the cache.
        NimboCore.refreshNotifications()

        val items = NimboCore.notifications().getOrElse { error ->
            // A failed refresh must not clear the record of what we have shown,
            // or the next success would re-announce everything.
            Log.w(TAG, "could not refresh server notifications", error)
            return
        }

        val prefs = context.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
        val seen: Set<Int>? =
            if (!prefs.contains(KEY_SEEN)) null
            else prefs.getStringSet(KEY_SEEN, emptySet()).orEmpty().mapNotNull { it.toIntOrNull() }.toSet()

        val plan = NotificationPlan.of(seen, items)

        plan.toPost.forEach { item ->
            Notifications.showServerNotification(
                context = context,
                serverId = item.id,
                title = item.subject.ifBlank { item.app.ifBlank { "Nextcloud" } },
                message = item.message,
            )
        }
        plan.toCancel.forEach { id -> Notifications.cancelServerNotification(context, id) }

        runCatching {
            prefs.edit { putStringSet(KEY_SEEN, plan.toRemember.map { it.toString() }.toSet()) }
        }.onFailure { Log.w(TAG, "could not record seen notifications", it) }
    }

    /**
     * Forgets what has been shown, so the next sync treats the account as new
     * and announces nothing. Called on sign-out: the next person to sign in
     * should not inherit someone else's shade.
     */
    fun forget(context: Context) {
        runCatching {
            context.getSharedPreferences(PREFS, Context.MODE_PRIVATE).edit { clear() }
        }.onFailure { Log.w(TAG, "could not clear seen notifications", it) }
    }
}
