/*
 * AppIconSwitcher.kt — shows one launcher alias and hides the rest.
 *
 * The new alias is enabled before the others are disabled, so there is never
 * a moment with no launcher entry. Does nothing when the right alias is
 * already the only one showing, so launchers aren't poked on every start.
 */
package dev.otherworld.nimbo.supporter

import android.content.ComponentName
import android.content.Context
import android.content.pm.PackageManager
import android.util.Log

object AppIconSwitcher {

    fun apply(context: Context, icon: AppIcon) {
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
        }.onFailure { Log.w("NimboSupporter", "could not switch the app icon", it) }
    }
}
