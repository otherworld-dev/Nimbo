/*
 * EngineListener.kt — the Go core's callback sink.
 *
 * HARD RULE (MOBILE_API.md): gomobile generates no exception check for these
 * void callbacks, so a Kotlin exception escaping any of them kills the process.
 * Every body is therefore wrapped in runCatching and swallows.
 *
 * Callbacks arrive on Go-owned threads. Writing MutableStateFlow.value and
 * tryEmit() is thread-safe, so we deliberately do NOT hop to the main thread
 * here — Compose collection handles that. Nothing in here blocks: the one call
 * that needs the core (re-reading the pause flag) is dispatched to IO.
 */
package dev.otherworld.nimbo.core

import android.util.Log
import dev.otherworld.mobile.Listener
import kotlinx.coroutines.flow.update
import kotlinx.serialization.decodeFromString

internal object EngineListener : Listener {

    private const val TAG = "NimboListener"

    override fun onStatus(status: String) {
        runCatching {
            NimboCore.statusState.value = status
        }.onFailure { Log.w(TAG, "onStatus", it) }
    }

    override fun onProgress(progressJSON: String) {
        runCatching {
            val text = progressJSON.trim()
            NimboCore.progressState.value = if (text.isEmpty() || text == "null") {
                null
            } else {
                NimboJson.decodeFromString<SyncProgress>(text)
            }
        }.onFailure { Log.w(TAG, "onProgress", it) }
    }

    override fun onPauseChanged() {
        runCatching {
            // isPaused() is a blocking core call — never make it on a Go thread.
            NimboCore.refreshPausedAsync()
        }.onFailure { Log.w(TAG, "onPauseChanged", it) }
    }

    override fun onPairSynced(localDir: String, remoteRoot: String, statsJSON: String) {
        runCatching {
            val text = statsJSON.trim()
            val stats = if (text.isEmpty() || text == "null") {
                SyncStats()
            } else {
                NimboJson.decodeFromString<SyncStats>(text)
            }
            val now = System.currentTimeMillis()
            // Every completed pass updates "when we last checked"...
            NimboCore.lastSyncAtState.value = now
            // ...but only a pass that moved something replaces what is shown as
            // the last sync. The engine fires this for no-op polls too, and those
            // used to wipe a real result to zeros within seconds of it appearing.
            if (stats.hasActivity) {
                NimboCore.lastPairSyncedState.value =
                    PairSyncedEvent(localDir, remoteRoot, stats, now)
            }
        }.onFailure { Log.w(TAG, "onPairSynced", it) }
    }

    override fun onConflictsChanged() {
        runCatching {
            NimboCore.conflictsChangedState.update { it + 1 }
        }.onFailure { Log.w(TAG, "onConflictsChanged", it) }
    }

    /** NOTE: the count is a Long on the Go side. */
    override fun onNotificationsChanged(count: Long) {
        runCatching {
            NimboCore.notificationCountState.value = count
        }.onFailure { Log.w(TAG, "onNotificationsChanged", it) }
    }

    override fun onToast(title: String, message: String, link: String) {
        runCatching {
            NimboCore.toastsState.tryEmit(ToastEvent(title, message, link))
        }.onFailure { Log.w(TAG, "onToast", it) }
    }

    override fun onAuthLost() {
        runCatching {
            NimboCore.authLostState.value = true
        }.onFailure { Log.w(TAG, "onAuthLost", it) }
    }
}
