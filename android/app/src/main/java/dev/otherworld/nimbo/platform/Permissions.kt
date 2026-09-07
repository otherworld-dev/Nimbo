/*
 * Permissions.kt — the three platform grants Nimbo needs, and the settings
 * intents that let the user give them.
 *
 * "All files access" and the battery-optimisation exemption are not runtime
 * permission dialogs: they are settings screens, so we hand back Intents and
 * the caller launches them (wrapped in try/catch — some OEM builds simply do
 * not ship the screen).
 */
package dev.otherworld.nimbo.platform

import android.Manifest
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Build
import android.os.Environment
import android.os.PowerManager
import android.provider.Settings
import androidx.core.content.ContextCompat

object Permissions {

    /**
     * True when the app may read/write the whole shared storage volume.
     *
     * Below API 30 there is no such toggle — the legacy storage permissions
     * declared in the manifest cover it — so this reports true and the
     * permissions screen simply shows the row as satisfied.
     */
    fun hasAllFilesAccess(): Boolean {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.R) return true
        return runCatching { Environment.isExternalStorageManager() }.getOrDefault(false)
    }

    /**
     * Settings screen for "All files access". Prefers the per-app screen; falls
     * back to the global list, then to this app's details page, picking the
     * first that something on the device can actually handle.
     */
    fun allFilesAccessIntent(context: Context): Intent {
        val appDetails = Intent(
            Settings.ACTION_APPLICATION_DETAILS_SETTINGS,
            packageUri(context),
        ).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)

        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.R) return appDetails

        val perApp = Intent(
            Settings.ACTION_MANAGE_APP_ALL_FILES_ACCESS_PERMISSION,
            packageUri(context),
        ).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
        if (canHandle(context, perApp)) return perApp

        val global = Intent(Settings.ACTION_MANAGE_ALL_FILES_ACCESS_PERMISSION)
            .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
        if (canHandle(context, global)) return global

        return appDetails
    }

    /** POST_NOTIFICATIONS; implicitly granted below API 33. */
    fun hasNotifications(context: Context): Boolean {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.TIRAMISU) return true
        return ContextCompat.checkSelfPermission(
            context,
            Manifest.permission.POST_NOTIFICATIONS,
        ) == PackageManager.PERMISSION_GRANTED
    }

    /** True when Doze/App Standby will leave the push socket alone. */
    fun isIgnoringBatteryOptimizations(context: Context): Boolean = runCatching {
        val power = context.getSystemService(Context.POWER_SERVICE) as? PowerManager
        power?.isIgnoringBatteryOptimizations(context.packageName) ?: false
    }.getOrDefault(false)

    /**
     * The "ask to ignore battery optimisations" dialog, falling back to the
     * full battery-optimisation settings list where the direct request is not
     * available.
     */
    fun batteryOptimizationIntent(context: Context): Intent {
        val request = Intent(
            Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS,
            packageUri(context),
        ).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
        if (canHandle(context, request)) return request

        val list = Intent(Settings.ACTION_IGNORE_BATTERY_OPTIMIZATION_SETTINGS)
            .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
        if (canHandle(context, list)) return list

        return Intent(
            Settings.ACTION_APPLICATION_DETAILS_SETTINGS,
            packageUri(context),
        ).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
    }

    // ---- internals ----------------------------------------------------------

    private fun packageUri(context: Context): Uri =
        Uri.parse("package:" + context.packageName)

    private fun canHandle(context: Context, intent: Intent): Boolean = runCatching {
        intent.resolveActivity(context.packageManager) != null
    }.getOrDefault(false)
}
