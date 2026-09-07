/*
 * Notifications.kt — every notification the app posts lives here.
 *
 * Two channels: a silent low-importance one for the foreground-service status
 * notification that mirrors the engine's Listener.onStatus, and a default one
 * for engine alerts (Listener.onToast). Nothing in here throws: alerts are
 * simply dropped when the user has notifications turned off.
 */
package dev.otherworld.nimbo.service

import android.annotation.SuppressLint
import android.app.Notification
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.net.Uri
import android.util.Log
import androidx.core.app.NotificationChannelCompat
import androidx.core.app.NotificationCompat
import androidx.core.app.NotificationManagerCompat
import dev.otherworld.nimbo.MainActivity
import dev.otherworld.nimbo.R
import dev.otherworld.nimbo.platform.Permissions
import java.util.concurrent.atomic.AtomicInteger

object Notifications {

    const val CHANNEL_SYNC = "sync"
    const val CHANNEL_ALERTS = "alerts"

    /** Notifications raised by the SERVER — shares, mentions, app messages. */
    const val CHANNEL_SERVER = "server"
    const val NOTIF_ID_FOREGROUND = 1

    private const val TAG = "NimboNotifications"

    /** Alert notification ids start well clear of NOTIF_ID_FOREGROUND. */
    private val alertIds = AtomicInteger(1000)

    private const val REQ_CONTENT = 100
    private const val REQ_SYNC_NOW = 101
    private const val REQ_ALERT_BASE = 1000

    /** Immutable is required from API 31 and harmless below it. */
    private const val PI_FLAGS = PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE

    /**
     * Creates (or updates) both channels. Safe to call repeatedly — the
     * platform treats channel creation as idempotent. Called from
     * Application.onCreate and again from SyncService.onCreate.
     */
    fun ensureChannels(context: Context) {
        runCatching {
            val sync = NotificationChannelCompat
                .Builder(CHANNEL_SYNC, NotificationManagerCompat.IMPORTANCE_LOW)
                .setName("Sync status")
                .setDescription("Ongoing sync activity and progress")
                .setSound(null, null)
                .setVibrationEnabled(false)
                .setShowBadge(false)
                .build()

            val alerts = NotificationChannelCompat
                .Builder(CHANNEL_ALERTS, NotificationManagerCompat.IMPORTANCE_DEFAULT)
                .setName("Sync alerts")
                .setDescription("Sync errors, conflicts and blocked files")
                .setShowBadge(true)
                .build()

            // Separate from "alerts" on purpose: sync problems are Nimbo's own
            // business, while these come from Nextcloud. A user who wants one
            // and not the other can silence either without losing both.
            val server = NotificationChannelCompat
                .Builder(CHANNEL_SERVER, NotificationManagerCompat.IMPORTANCE_DEFAULT)
                .setName("Nextcloud notifications")
                .setDescription("Shares, mentions and messages from your server")
                .setShowBadge(true)
                .build()

            NotificationManagerCompat.from(context)
                .createNotificationChannelsCompat(listOf(sync, alerts, server))
        }.onFailure { Log.w(TAG, "ensureChannels failed", it) }
    }

    /**
     * The ongoing foreground-service notification. [progressPercent] in 0..100
     * draws a determinate bar; null draws none.
     */
    fun buildForeground(context: Context, status: String, progressPercent: Int?): Notification {
        val builder = NotificationCompat.Builder(context, CHANNEL_SYNC)
            .setSmallIcon(R.drawable.ic_notification)
            .setContentTitle("Nimbo")
            .setContentText(status.ifBlank { "Working…" })
            .setContentIntent(mainActivityIntent(context, REQ_CONTENT))
            .setOngoing(true)
            .setSilent(true)
            .setOnlyAlertOnce(true)
            .setShowWhen(false)
            .setPriority(NotificationCompat.PRIORITY_LOW)
            .setCategory(NotificationCompat.CATEGORY_SERVICE)
            .setForegroundServiceBehavior(NotificationCompat.FOREGROUND_SERVICE_IMMEDIATE)
            .addAction(
                R.drawable.ic_notification,
                "Sync now",
                syncNowIntent(context),
            )

        if (progressPercent != null) {
            builder.setProgress(100, progressPercent.coerceIn(0, 100), false)
        }
        return builder.build()
    }

    /**
     * Posts an engine alert. No-ops (rather than throwing) when the user has
     * notifications disabled or POST_NOTIFICATIONS was never granted.
     */
    @SuppressLint("MissingPermission")
    fun showAlert(context: Context, title: String, message: String, link: String) {
        runCatching {
            val manager = NotificationManagerCompat.from(context)
            if (!manager.areNotificationsEnabled()) return
            if (!Permissions.hasNotifications(context)) return

            val id = nextAlertId()
            val notification = NotificationCompat.Builder(context, CHANNEL_ALERTS)
                .setSmallIcon(R.drawable.ic_notification)
                .setContentTitle(title.ifBlank { "Nimbo" })
                .setContentText(message)
                .setStyle(NotificationCompat.BigTextStyle().bigText(message))
                .setContentIntent(alertContentIntent(context, id, link))
                .setAutoCancel(true)
                .setPriority(NotificationCompat.PRIORITY_DEFAULT)
                .build()

            manager.notify(id, notification)
        }.onFailure { Log.w(TAG, "showAlert failed", it) }
    }

    /**
     * Mirrors one server notification into the shade. Tapping opens Nimbo on
     * its notifications screen; the server's own link is deliberately NOT
     * opened in a browser here, unlike an engine alert — these are things the
     * app itself can now show.
     */
    @SuppressLint("MissingPermission")
    fun showServerNotification(context: Context, serverId: Int, title: String, message: String) {
        runCatching {
            val manager = NotificationManagerCompat.from(context)
            if (!manager.areNotificationsEnabled()) return
            if (!Permissions.hasNotifications(context)) return

            val body = message.ifBlank { "" }
            val builder = NotificationCompat.Builder(context, CHANNEL_SERVER)
                .setSmallIcon(R.drawable.ic_notification)
                .setContentTitle(title.ifBlank { "Nextcloud" })
                .setContentIntent(notificationsScreenIntent(context, serverId))
                .setAutoCancel(true)
                .setGroup(GROUP_SERVER)
                .setPriority(NotificationCompat.PRIORITY_DEFAULT)
            if (body.isNotBlank()) {
                builder.setContentText(body).setStyle(NotificationCompat.BigTextStyle().bigText(body))
            }
            manager.notify(serverNotificationId(serverId), builder.build())
        }.onFailure { Log.w(TAG, "showServerNotification failed", it) }
    }

    /** Takes one back down — it was dismissed in the app, or on another device. */
    fun cancelServerNotification(context: Context, serverId: Int) {
        runCatching {
            NotificationManagerCompat.from(context).cancel(serverNotificationId(serverId))
        }.onFailure { Log.w(TAG, "cancelServerNotification failed", it) }
    }

    /**
     * A stable Android id for a server notification, so the same notification
     * updates rather than duplicating, and can be cancelled later by id alone.
     *
     * Kept in its own numeric range, clear of the foreground notification and
     * the alert ids.
     */
    fun serverNotificationId(serverId: Int): Int =
        SERVER_ID_BASE + (Math.floorMod(serverId, SERVER_ID_SPAN))

    private const val GROUP_SERVER = "dev.otherworld.nimbo.SERVER"
    private const val SERVER_ID_BASE = 2_000_000
    private const val SERVER_ID_SPAN = 1_000_000

    private fun notificationsScreenIntent(context: Context, id: Int): PendingIntent {
        val intent = Intent(context, MainActivity::class.java)
            .setAction(Intent.ACTION_MAIN)
            .addCategory(Intent.CATEGORY_LAUNCHER)
            .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_CLEAR_TOP)
            .putExtra(MainActivity.EXTRA_OPEN_NOTIFICATIONS, true)
            .putExtra(MainActivity.EXTRA_NOTIFICATION_ID, id)
        // The request code is unique per notification, so these PendingIntents
        // stay distinct rather than the newest overwriting the rest.
        return PendingIntent.getActivity(context, serverNotificationId(id), intent, PI_FLAGS)
    }

    // ---- internals ----------------------------------------------------------

    private fun nextAlertId(): Int {
        // Wrap well before Int overflow so ids stay positive and distinct from
        // the foreground notification.
        val next = alertIds.incrementAndGet()
        if (next > 100_000) alertIds.set(REQ_ALERT_BASE)
        return next
    }

    private fun mainActivityIntent(context: Context, requestCode: Int): PendingIntent {
        val intent = Intent(context, MainActivity::class.java)
            .setAction(Intent.ACTION_MAIN)
            .addCategory(Intent.CATEGORY_LAUNCHER)
            .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_CLEAR_TOP)
        return PendingIntent.getActivity(context, requestCode, intent, PI_FLAGS)
    }

    private fun alertContentIntent(context: Context, id: Int, link: String): PendingIntent {
        if (link.isNotBlank()) {
            val browser = runCatching {
                val view = Intent(Intent.ACTION_VIEW, Uri.parse(link))
                    .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
                if (view.resolveActivity(context.packageManager) == null) {
                    null
                } else {
                    PendingIntent.getActivity(context, id, view, PI_FLAGS)
                }
            }.getOrNull()
            if (browser != null) return browser
        }
        return mainActivityIntent(context, id)
    }

    /**
     * Tapping "Sync now" delivers ACTION_SYNC_NOW to the service. The action is
     * only reachable while the service is in the foreground, so
     * getForegroundService() is always a legal start.
     */
    private fun syncNowIntent(context: Context): PendingIntent {
        val intent = Intent(context, SyncService::class.java)
            .setAction(SyncService.ACTION_SYNC_NOW)
        return PendingIntent.getForegroundService(context, REQ_SYNC_NOW, intent, PI_FLAGS)
    }
}
