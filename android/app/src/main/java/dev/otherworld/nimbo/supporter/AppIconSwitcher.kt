/*
 * AppIconSwitcher.kt — shows one launcher alias and hides the rest.
 *
 * The new alias is enabled before the others are disabled, so there is never
 * a moment with no launcher entry. Does nothing when the right alias is
 * already the only one showing, so launchers aren't poked on every start.
 *
 * Disabling the alias a task was launched from makes Android close the task,
 * so the switch waits for a moment when nobody is in it (IconSwitchRules.kt).
 * It runs in Supporter.scope, not a screen's ViewModel, so leaving with Back
 * cannot cancel it.
 *
 * A switch that has to wait is not stored anywhere: the chosen icon is in
 * SupporterPrefs and the aliases say what is showing, so it is worked out
 * again the next time the last screen stops or the process starts.
 */
package dev.otherworld.nimbo.supporter

import android.app.Activity
import android.app.ActivityManager
import android.app.Application
import android.content.ComponentName
import android.content.Context
import android.content.pm.PackageManager
import android.os.Bundle
import android.os.PowerManager
import android.util.Log
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

object AppIconSwitcher {

    private const val TAG = "NimboSupporter"

    /**
     * How long after the last screen stops, or the process starts, before
     * looking. A screen that was already on its way (the launch that started
     * the process, a quick return) has started by then and holds the switch
     * back.
     */
    private const val SETTLE_MS = 1_000L

    // Main thread only.
    private var startedScreens = 0
    private var screenWentOff = false
    private var waiting: Job? = null

    /** Once per process, from Supporter.init. */
    fun watch(app: Application) {
        app.registerActivityLifecycleCallbacks(object : Application.ActivityLifecycleCallbacks {
            override fun onActivityStarted(activity: Activity) {
                startedScreens++
            }

            override fun onActivityStopped(activity: Activity) {
                startedScreens = (startedScreens - 1).coerceAtLeast(0)
                // A rotation stops the screen and starts it again: nobody left.
                if (startedScreens > 0 || activity.isChangingConfigurations) return
                screenWentOff = app.getSystemService(PowerManager::class.java)?.isInteractive == false
                switchWhenSafe(app)
            }

            override fun onActivityCreated(activity: Activity, savedInstanceState: Bundle?) = Unit
            override fun onActivityResumed(activity: Activity) = Unit
            override fun onActivityPaused(activity: Activity) = Unit
            override fun onActivitySaveInstanceState(activity: Activity, outState: Bundle) = Unit
            override fun onActivityDestroyed(activity: Activity) = Unit
        })
        // A switch that was still waiting when the process last ended.
        switchWhenSafe(app)
    }

    /**
     * The icon follows the perks (a lapsed Backer gets the default back), and
     * is read when the switch happens, so the latest choice wins.
     */
    private fun switchWhenSafe(app: Application) {
        waiting?.cancel()
        waiting = Supporter.scope.launch {
            delay(SETTLE_MS)
            if (!canSwitchIcon(startedScreens, screenWentOff, otherAppOnTop(app))) return@launch
            val icon = effectiveIcon(SupporterPrefs(app).icon, Perks.of(Supporter.repository.status.value.tier))
            withContext(Dispatchers.IO) { apply(app, icon) }
        }
    }

    /** Not knowing is a reason to wait, not to risk closing what the user is in. */
    private fun otherAppOnTop(context: Context): Boolean =
        runCatching {
            val tasks = context.getSystemService(ActivityManager::class.java).appTasks
            hasOtherAppOnTop(context.packageName, tasks.map { it.taskInfo.topActivity?.packageName })
        }.getOrElse {
            Log.w(TAG, "could not look at Nimbo's tasks", it)
            true
        }

    private fun apply(context: Context, icon: AppIcon) {
        runCatching {
            val pm = context.packageManager
            fun component(i: AppIcon) = ComponentName(context.packageName, i.aliasName)
            fun enabled(i: AppIcon) = when (pm.getComponentEnabledSetting(component(i))) {
                PackageManager.COMPONENT_ENABLED_STATE_ENABLED -> true
                PackageManager.COMPONENT_ENABLED_STATE_DISABLED -> false
                // Never changed: whatever the manifest says (only the default is on).
                else -> i == AppIcon.DEFAULT
            }

            if (AppIcon.entries.all { enabled(it) == (it == icon) }) return

            pm.setComponentEnabledSetting(
                component(icon),
                PackageManager.COMPONENT_ENABLED_STATE_ENABLED,
                PackageManager.DONT_KILL_APP,
            )
            AppIcon.entries.filter { it != icon }.forEach {
                pm.setComponentEnabledSetting(
                    component(it),
                    PackageManager.COMPONENT_ENABLED_STATE_DISABLED,
                    PackageManager.DONT_KILL_APP,
                )
            }
        }.onFailure { Log.w(TAG, "could not switch the app icon", it) }
    }
}
