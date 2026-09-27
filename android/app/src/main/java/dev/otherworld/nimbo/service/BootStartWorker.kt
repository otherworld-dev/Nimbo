/*
 * BootStartWorker.kt — starts SyncService after a reboot, once there is a
 * network to start the engine against (see BootReceiver for why the receiver
 * doesn't do this itself).
 *
 * Android can still refuse the start. Measured on Android 15 and 16 emulators:
 * the service starts when the app ignores battery optimisation, and without
 * that startForeground throws "FGS type dataSync not allowed to start from
 * BOOT_COMPLETED" even though the start comes from this worker, not the
 * receiver. Either way the refusal ends in a notification asking the user to
 * tap it, which opens the app and starts sync the normal way (SyncService
 * posts it when the refusal comes from startForeground). The periodic
 * SyncWorker keeps syncing every 15 minutes meanwhile.
 */
package dev.otherworld.nimbo.service

import android.content.Context
import android.util.Log
import androidx.work.Constraints
import androidx.work.CoroutineWorker
import androidx.work.ExistingWorkPolicy
import androidx.work.NetworkType
import androidx.work.OneTimeWorkRequestBuilder
import androidx.work.WorkManager
import androidx.work.WorkerParameters
import dev.otherworld.nimbo.core.NimboCore
import dev.otherworld.nimbo.platform.Permissions

class BootStartWorker(
    appContext: Context,
    params: WorkerParameters,
) : CoroutineWorker(appContext, params) {

    override suspend fun doWork(): Result {
        // Checked again: the user may have signed out between boot and now.
        if (!NimboCore.hasAccount() || !Permissions.hasAllFilesAccess()) return Result.success()
        if (SyncService.tryStart(applicationContext)) {
            Log.i(TAG, "asked for the sync service after boot")
        } else {
            Log.w(TAG, "Android refused to start the sync service after boot")
            Notifications.showResumeSync(applicationContext)
        }
        return Result.success()
    }

    companion object {
        private const val TAG = "NimboBoot"
        private const val UNIQUE_NAME = "nimbo-boot-start"

        fun enqueue(context: Context) {
            runCatching {
                val request = OneTimeWorkRequestBuilder<BootStartWorker>()
                    .setConstraints(
                        Constraints.Builder()
                            .setRequiredNetworkType(NetworkType.CONNECTED)
                            .build(),
                    )
                    .build()
                WorkManager.getInstance(context)
                    .enqueueUniqueWork(UNIQUE_NAME, ExistingWorkPolicy.REPLACE, request)
            }.onFailure { Log.w(TAG, "enqueue failed", it) }
        }
    }
}
