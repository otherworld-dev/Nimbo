/*
 * BootReceiver.kt — brings sync back after the phone restarts.
 *
 * Without it the foreground service (and with it the watcher and push) stayed
 * down until the user next opened the app; only the 15-minute WorkManager pass
 * survived a reboot.
 *
 * The receiver does not start SyncService itself, for two reasons:
 *  - Android 15+ refuses to start a `dataSync` foreground service from a
 *    BOOT_COMPLETED receiver (ForegroundServiceStartNotAllowedException).
 *  - The network is often not up yet at BOOT_COMPLETED, and an engine start
 *    that can't reach the server fails, alerts and shuts the service down.
 * So it hands over to BootStartWorker, which waits for a network first.
 */
package dev.otherworld.nimbo.service

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.util.Log
import dev.otherworld.nimbo.core.NimboCore
import dev.otherworld.nimbo.platform.Permissions

class BootReceiver : BroadcastReceiver() {

    override fun onReceive(context: Context, intent: Intent) {
        if (intent.action != Intent.ACTION_BOOT_COMPLETED) return
        // The same conditions the app starts the service on: signed in, and
        // allowed to write the synced folders.
        if (!NimboCore.hasAccount() || !Permissions.hasAllFilesAccess()) {
            Log.i(TAG, "boot: not signed in or no all-files access, not starting sync")
            return
        }
        Log.i(TAG, "boot: starting sync once the network is up")
        BootStartWorker.enqueue(context)
    }

    private companion object {
        const val TAG = "NimboBoot"
    }
}
