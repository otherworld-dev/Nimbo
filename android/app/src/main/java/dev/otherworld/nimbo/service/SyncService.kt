/*
 * SyncService.kt — the foreground service that hosts the Go engine's run loop.
 *
 * It owns nothing but the Android side of the contract: go foreground within
 * the 5-second window, start the engine off the main thread, mirror the
 * engine's status/progress into the ongoing notification, and turn engine
 * toasts into alert notifications.
 */
package dev.otherworld.nimbo.service

import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.os.Build
import android.os.IBinder
import android.util.Log
import androidx.annotation.RequiresApi
import androidx.core.app.NotificationManagerCompat
import androidx.core.app.ServiceCompat
import androidx.core.content.ContextCompat
import androidx.lifecycle.LifecycleService
import androidx.lifecycle.lifecycleScope
import dev.otherworld.nimbo.core.NimboCore
import dev.otherworld.nimbo.core.SyncProgress
import kotlinx.coroutines.DelicateCoroutinesApi
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.GlobalScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.conflate
import kotlinx.coroutines.launch

class SyncService : LifecycleService() {

    companion object {
        const val ACTION_START = "dev.otherworld.nimbo.START"
        const val ACTION_STOP = "dev.otherworld.nimbo.STOP"
        const val ACTION_SYNC_NOW = "dev.otherworld.nimbo.SYNC_NOW"

        private const val TAG = "NimboSyncService"

        /** Minimum wall-clock gap between foreground-notification updates. */
        private const val NOTIFICATION_MIN_INTERVAL_MS = 1_000L

        /**
         * How often to re-read server notifications regardless of push.
         * One cheap OCS request: rare enough not to matter for battery, often
         * enough that a dead push channel does not mean a notification is
         * missed until the app is next restarted.
         */
        private const val NOTIFICATION_POLL_INTERVAL_MS = 5 * 60_000L

        fun start(context: Context) = send(context, ACTION_START)

        fun stop(context: Context) = send(context, ACTION_STOP)

        fun syncNow(context: Context) = send(context, ACTION_SYNC_NOW)

        private fun send(context: Context, action: String) {
            val intent = Intent(context, SyncService::class.java).setAction(action)
            // Android 12+ can refuse a background foreground-service start
            // (ForegroundServiceStartNotAllowedException). That is a refusal to
            // start work, not a reason to take the caller down with us.
            runCatching { ContextCompat.startForegroundService(context, intent) }
                .onFailure { Log.w(TAG, "startForegroundService($action) refused", it) }
        }
    }

    /** Set once startForeground has succeeded; guards notification updates. */
    @Volatile
    private var inForeground = false

    /** Set when an explicit stop is in flight so we stop repainting. */
    @Volatile
    private var stopping = false

    private var collectorsStarted = false
    private var engineJob: Job? = null

    override fun onCreate() {
        super.onCreate()
        Notifications.ensureChannels(this)
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        super.onStartCommand(intent, flags, startId)

        // FIRST, before any engine work: NimboCore.startEngine() blocks on the
        // network and the 5-second startForeground window is unforgiving.
        goForeground("Starting…", null)

        when (intent?.action) {
            ACTION_STOP -> {
                handleStop()
                return START_NOT_STICKY
            }

            ACTION_SYNC_NOW -> {
                startCollectors()
                requestSyncNow()
            }

            else -> {
                startCollectors()
                ensureEngineStarted()
            }
        }
        return START_STICKY
    }

    override fun onBind(intent: Intent): IBinder? {
        // LifecycleService requires the super call to dispatch its lifecycle event.
        super.onBind(intent)
        return null
    }

    /**
     * Android 15+ caps a `dataSync` foreground service at roughly six hours a
     * day. When the budget runs out the system calls this, and a service that
     * does not stop itself within a few seconds is killed with an ANR — after
     * which the app is barred from starting a dataSync FGS until the window
     * resets. So: stop now, and do not wait for the engine to drain first.
     *
     * The 15-minute WorkManager cadence remains as the baseline, which is
     * exactly the situation it exists for.
     */
    @RequiresApi(Build.VERSION_CODES.VANILLA_ICE_CREAM)
    override fun onTimeout(startId: Int, fgsType: Int) {
        Log.w(TAG, "foreground service timed out (type=$fgsType) — stopping")
        handleTimeout()
    }

    /** The Android 14 single-argument form, for completeness. */
    @RequiresApi(Build.VERSION_CODES.UPSIDE_DOWN_CAKE)
    override fun onTimeout(startId: Int) {
        Log.w(TAG, "foreground service timed out — stopping")
        handleTimeout()
    }

    @OptIn(DelicateCoroutinesApi::class)
    private fun handleTimeout() {
        stopping = true
        // Detached: stopSelf() must happen inside the system's few-second grace
        // period, and Client.stop() can take up to ~30s to drain.
        GlobalScope.launch(Dispatchers.IO) {
            runCatching { NimboCore.stopEngine() }
                .onFailure { Log.w(TAG, "stopEngine after timeout failed", it) }
        }
        runCatching { ServiceCompat.stopForeground(this, ServiceCompat.STOP_FOREGROUND_REMOVE) }
        stopSelf()
    }

    @OptIn(DelicateCoroutinesApi::class)
    override fun onDestroy() {
        // lifecycleScope is cancelled as part of teardown, so the engine stop
        // cannot run on it. It has to complete even though this service is
        // going away, and it is bounded (Client.stop() returns within ~30s),
        // so a detached IO coroutine is the right tool here.
        GlobalScope.launch(Dispatchers.IO) {
            runCatching { NimboCore.stopEngine() }
                .onFailure { Log.w(TAG, "stopEngine on destroy failed", it) }
        }
        super.onDestroy()
    }

    // ---- engine ------------------------------------------------------------

    private fun ensureEngineStarted() {
        val running = engineJob
        if (running != null && running.isActive) return
        engineJob = lifecycleScope.launch {
            val result = NimboCore.startEngine()
            result.onFailure { error ->
                Log.w(TAG, "startEngine failed", error)
                Notifications.showAlert(
                    this@SyncService,
                    "Nimbo could not start",
                    error.message ?: "The sync engine failed to start.",
                    "",
                )
                shutdown()
            }
        }
    }

    /**
     * ACTION_SYNC_NOW never restarts the engine — it only makes sure it is up
     * (startEngine is a no-op when already running) and then asks for a pass.
     */
    private fun requestSyncNow() {
        lifecycleScope.launch {
            if (!NimboCore.isRunning()) {
                val started = NimboCore.startEngine()
                if (started.isFailure) {
                    val error = started.exceptionOrNull()
                    Log.w(TAG, "startEngine (for sync now) failed", error)
                    Notifications.showAlert(
                        this@SyncService,
                        "Nimbo could not start",
                        error?.message ?: "The sync engine failed to start.",
                        "",
                    )
                    shutdown()
                    return@launch
                }
            }
            NimboCore.syncNow().onFailure { error ->
                Log.w(TAG, "syncNow failed", error)
                Notifications.showAlert(
                    this@SyncService,
                    "Sync failed to start",
                    error.message ?: "Could not start a sync.",
                    "",
                )
            }
        }
    }

    private fun handleStop() {
        if (stopping) return
        stopping = true
        lifecycleScope.launch {
            runCatching { NimboCore.stopEngine() }
                .onFailure { Log.w(TAG, "stopEngine failed", it) }
            shutdown()
        }
    }

    private fun shutdown() {
        stopping = true
        runCatching { ServiceCompat.stopForeground(this, ServiceCompat.STOP_FOREGROUND_REMOVE) }
        stopSelf()
    }

    // ---- notification ------------------------------------------------------

    private fun startCollectors() {
        if (collectorsStarted) return
        collectorsStarted = true

        // Status + progress -> the ongoing notification. conflate() keeps only
        // the newest value while the collector sleeps, so a fast engine cannot
        // outrun the notification manager: at most one update per second, and
        // the final value is always painted.
        lifecycleScope.launch {
            combine(NimboCore.status, NimboCore.progress) { status, progress ->
                status to progress
            }.conflate().collect { (status, progress) ->
                updateForeground(status, progress)
                delay(NOTIFICATION_MIN_INTERVAL_MS)
            }
        }

        // Server notifications -> the Android shade. The callback carries only
        // a count, so each change triggers a re-read and a diff.
        lifecycleScope.launch {
            NimboCore.notificationCount.collect {
                ServerNotifications.sync(this@SyncService)
            }
        }

        // ...and a poll, because that callback fires off the back of a push
        // event. A websocket the client still believes in but which has quietly
        // died is indistinguishable from a quiet server, and the engine's own
        // post-sync fallback is disabled whenever push is merely AVAILABLE. One
        // cheap request every few minutes is the difference between "eventually"
        // and "never".
        lifecycleScope.launch {
            while (true) {
                delay(NOTIFICATION_POLL_INTERVAL_MS)
                ServerNotifications.sync(this@SyncService)
            }
        }

        // Engine toasts -> alert notifications.
        lifecycleScope.launch {
            NimboCore.toasts.collect { toast ->
                Notifications.showAlert(
                    this@SyncService,
                    toast.title,
                    toast.message,
                    toast.link,
                )
            }
        }
    }

    private fun goForeground(status: String, progressPercent: Int?) {
        val notification = Notifications.buildForeground(this, status, progressPercent)
        runCatching {
            ServiceCompat.startForeground(
                this,
                Notifications.NOTIF_ID_FOREGROUND,
                notification,
                ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC,
            )
            inForeground = true
        }.onFailure { Log.w(TAG, "startForeground failed", it) }
    }

    private fun updateForeground(status: String, progress: SyncProgress?) {
        if (!inForeground || stopping) return
        val text = notificationText(status, progress)
        val notification = Notifications.buildForeground(this, text, percentOf(progress))
        // Posting the same id updates the foreground notification in place.
        // Without POST_NOTIFICATIONS on API 33+ this is silently dropped, which
        // is exactly the behaviour we want (the service keeps running).
        runCatching {
            NotificationManagerCompat.from(this)
                .notify(Notifications.NOTIF_ID_FOREGROUND, notification)
        }.onFailure { Log.w(TAG, "notification update failed", it) }
    }

    private fun notificationText(status: String, progress: SyncProgress?): String {
        val base = status.ifBlank { "Working…" }
        val current = progress?.current.orEmpty()
        return if (progress != null && progress.active && current.isNotBlank()) {
            "$base — $current"
        } else {
            base
        }
    }

    private fun percentOf(progress: SyncProgress?): Int? {
        if (progress == null || !progress.active || progress.enumerating) return null
        if (progress.totalBytes > 0) {
            return ((progress.doneBytes * 100) / progress.totalBytes).toInt().coerceIn(0, 100)
        }
        if (progress.total > 0) {
            return ((progress.done * 100) / progress.total).toInt().coerceIn(0, 100)
        }
        return null
    }
}
