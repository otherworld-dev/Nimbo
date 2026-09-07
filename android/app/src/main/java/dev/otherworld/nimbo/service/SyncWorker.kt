/*
 * SyncWorker.kt — the guaranteed baseline cadence.
 *
 * WorkManager's 15-minute floor bounds how stale the device can get when the
 * foreground service is not resident.
 *
 * The worker drives the sync itself rather than poking SyncService. Delegating
 * would mean calling startForegroundService() from a background context, which
 * Android 12+ refuses with ForegroundServiceStartNotAllowedException in exactly
 * the situation this worker exists for — the app not being in the foreground.
 * A Worker is already a legitimate background execution context with its own
 * ~10-minute window, so it can bring the engine up and run a pass directly.
 */
package dev.otherworld.nimbo.service

import android.content.Context
import android.util.Log
import androidx.work.Constraints
import androidx.work.CoroutineWorker
import androidx.work.ExistingPeriodicWorkPolicy
import androidx.work.NetworkType
import androidx.work.PeriodicWorkRequestBuilder
import androidx.work.WorkManager
import androidx.work.WorkerParameters
import dev.otherworld.nimbo.core.NimboCore
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import java.util.concurrent.TimeUnit

class SyncWorker(
    appContext: Context,
    params: WorkerParameters,
) : CoroutineWorker(appContext, params) {

    override suspend fun doWork(): Result = withContext(Dispatchers.IO) {
        // If the foreground service already holds the engine, a pass is all we
        // need — and all we should do, since restarting a running engine is an
        // error by contract.
        val started = if (NimboCore.isRunning()) {
            kotlin.Result.success(Unit)
        } else {
            NimboCore.startEngine()
        }

        started.fold(
            onSuccess = {
                NimboCore.syncNow().fold(
                    onSuccess = { Result.success() },
                    onFailure = { error ->
                        Log.w(TAG, "periodic syncNow failed", error)
                        Result.retry()
                    },
                )
            },
            onFailure = { error ->
                // Offline, captive portal, server down — all transient. Let
                // WorkManager back off rather than reporting a success that did
                // nothing.
                Log.w(TAG, "periodic startEngine failed", error)
                Result.retry()
            },
        )
    }

    companion object {
        private const val TAG = "NimboSyncWorker"
        const val UNIQUE_NAME = "nimbo-periodic-sync"

        fun enqueuePeriodic(context: Context) {
            runCatching {
                val constraints = Constraints.Builder()
                    .setRequiredNetworkType(NetworkType.CONNECTED)
                    .build()

                val request = PeriodicWorkRequestBuilder<SyncWorker>(15, TimeUnit.MINUTES)
                    .setConstraints(constraints)
                    .build()

                WorkManager.getInstance(context).enqueueUniquePeriodicWork(
                    UNIQUE_NAME,
                    ExistingPeriodicWorkPolicy.KEEP,
                    request,
                )
            }.onFailure { Log.w(TAG, "enqueuePeriodic failed", it) }
        }

        fun cancel(context: Context) {
            runCatching { WorkManager.getInstance(context).cancelUniqueWork(UNIQUE_NAME) }
                .onFailure { Log.w(TAG, "cancel failed", it) }
        }
    }
}
