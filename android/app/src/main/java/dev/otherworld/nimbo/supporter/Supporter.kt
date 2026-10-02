/*
 * Supporter.kt — the app-lifetime home of the supporter repository.
 *
 * Created once in NimboApp, so Play's billing connection (or the direct
 * build's key check) starts with the process and the first screen already
 * knows the cached tier. Nothing here touches the sync engine, and a failure
 * in any supporter task is logged, never fatal.
 */
package dev.otherworld.nimbo.supporter

import android.app.Application
import android.util.Log
import kotlinx.coroutines.CoroutineExceptionHandler
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.launch

object Supporter {
    private val handler = CoroutineExceptionHandler { _, t -> Log.w("NimboSupporter", "supporter task failed", t) }

    val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main.immediate + handler)

    lateinit var repository: SupporterRepository
        private set

    fun init(app: Application) {
        repository = createSupporterRepository(app, scope)
        scope.launch { repository.refresh(force = false) }
    }
}
