/*
 * MainActivity.kt — the single Activity. It hosts the Compose tree and owns
 * everything that genuinely needs an Activity:
 *
 *  - the Chrome Custom Tab used by Login Flow v2 (with an ACTION_VIEW fallback),
 *  - the POST_NOTIFICATIONS permission launcher,
 *  - the "All files access" / battery-exemption settings intents (every
 *    startActivity is wrapped: OEM builds do omit these screens),
 *  - an ON_RESUME observer that re-reads the permission states, so coming back
 *    from the All-files settings screen updates the UI instead of leaving it stale.
 *
 * The composables reach these through LocalNimboHost, which keeps NimboNav free
 * of Activity lookups while MainActivity keeps ownership of the launchers.
 */
package dev.otherworld.nimbo

import android.Manifest
import android.content.ClipData
import android.content.ClipboardManager
import android.content.Intent
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.provider.Settings
import android.util.Log
import android.widget.Toast
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import dev.otherworld.nimbo.ui.theme.resolveDark
import dev.otherworld.nimbo.ui.theme.parseThemeColor
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.compose.runtime.getValue
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.activity.result.contract.ActivityResultContracts
import androidx.activity.viewModels
import androidx.browser.customtabs.CustomTabsIntent
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.runtime.staticCompositionLocalOf
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.LifecycleEventObserver
import dev.otherworld.nimbo.platform.Permissions
import dev.otherworld.nimbo.ui.NimboNav
import dev.otherworld.nimbo.ui.NimboViewModel
import dev.otherworld.nimbo.ui.theme.NimboTheme

private const val TAG = "MainActivity"

/** Activity-backed actions the UI needs; implemented by [MainActivity]. */
interface NimboHost {
    /** Opens a URL in a Custom Tab (browser fallback), never throwing. */
    fun openUrl(url: String)

    /** Puts text on the clipboard, confirming it where the system does not. */
    fun copyToClipboard(text: String, confirmation: String)

    /** Hands a downloaded/synced file to whichever app can show it. */
    fun openFile(uri: Uri, mimeType: String)

    /** Opens the system document picker; the chosen file is uploaded. */
    fun pickFileToUpload()

    /** Asks for POST_NOTIFICATIONS, or opens the app's notification settings. */
    fun requestNotificationPermission()

    /** Opens the special "All files access" settings screen for this app. */
    fun openAllFilesSettings()

    /** Opens the battery-optimisation exemption prompt. */
    fun openBatteryExemptionSettings()
}

val LocalNimboHost = staticCompositionLocalOf<NimboHost> {
    error("No NimboHost provided; wrap the UI in CompositionLocalProvider(LocalNimboHost provides host)")
}

class MainActivity : ComponentActivity(), NimboHost {

    companion object {
        /** Set by a shade notification: open straight onto the notifications screen. */
        const val EXTRA_OPEN_NOTIFICATIONS = "dev.otherworld.nimbo.OPEN_NOTIFICATIONS"

        /** Which notification was tapped, so the list can point at it. */
        const val EXTRA_NOTIFICATION_ID = "dev.otherworld.nimbo.NOTIFICATION_ID"
    }


    private val viewModel: NimboViewModel by viewModels()

    private val notificationPermissionLauncher =
        registerForActivityResult(ActivityResultContracts.RequestPermission()) {
            // Whatever the answer, re-read the real state rather than trusting the flag.
            viewModel.refreshPermissions()
        }

    private val uploadPickerLauncher =
        registerForActivityResult(ActivityResultContracts.OpenDocument()) { uri ->
            // Null when the user backed out of the picker.
            if (uri != null) viewModel.uploadFrom(uri)
        }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)

        lifecycle.addObserver(
            LifecycleEventObserver { _, event ->
                if (event == Lifecycle.Event.ON_RESUME) {
                    viewModel.refreshPermissions()
                }
            }
        )

        handleIntent(intent)

        val host: NimboHost = this
        setContent {
            val state by viewModel.state.collectAsStateWithLifecycle()
            CompositionLocalProvider(LocalNimboHost provides host) {
                NimboTheme(
                    darkTheme = resolveDark(
                        preference = state.appearance,
                        serverAppearance = state.serverAppearance,
                        systemDark = isSystemInDarkTheme(),
                    ),
                    // The user's Nextcloud colour, as the desktop client does.
                    accent = parseThemeColor(state.themeColor),
                ) {
                    NimboNav(viewModel)
                }
            }
        }
    }

    /**
     * The activity is singleTop-ish via CLEAR_TOP, so a shade tap on a running
     * app arrives here rather than through onCreate.
     */
    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        setIntent(intent)
        handleIntent(intent)
    }

    /**
     * Routes a launch that came from a notification. Deliberately consumes the
     * extra: without that, every later resume would bounce the user back to the
     * notifications screen they had already navigated away from.
     */
    private fun handleIntent(intent: Intent?) {
        if (intent?.getBooleanExtra(EXTRA_OPEN_NOTIFICATIONS, false) != true) return
        val focus = intent.getIntExtra(EXTRA_NOTIFICATION_ID, 0).takeIf { it != 0 }
        intent.removeExtra(EXTRA_OPEN_NOTIFICATIONS)
        intent.removeExtra(EXTRA_NOTIFICATION_ID)
        viewModel.openNotifications(focus)
    }

    // ------------------------------------------------------------------ NimboHost

    override fun copyToClipboard(text: String, confirmation: String) {
        if (text.isBlank()) return
        try {
            val clipboard = getSystemService(ClipboardManager::class.java) ?: return
            clipboard.setPrimaryClip(ClipData.newPlainText("Nimbo", text))
            // Android 13+ shows its own copy confirmation; a second one is noise.
            if (Build.VERSION.SDK_INT < Build.VERSION_CODES.TIRAMISU) {
                Toast.makeText(this, confirmation, Toast.LENGTH_SHORT).show()
            }
        } catch (t: Throwable) {
            Log.w(TAG, "could not copy to clipboard", t)
        }
    }

    override fun openUrl(url: String) {
        if (url.isBlank()) return
        val uri = try {
            Uri.parse(url)
        } catch (t: Throwable) {
            Log.w(TAG, "unparseable url: $url", t)
            return
        }
        try {
            CustomTabsIntent.Builder()
                .setShowTitle(true)
                .build()
                .launchUrl(this, uri)
            return
        } catch (t: Throwable) {
            Log.w(TAG, "custom tab unavailable, falling back to ACTION_VIEW", t)
        }
        startActivitySafely(
            Intent(Intent.ACTION_VIEW, uri),
            "No browser is available to open this link",
        )
    }

    override fun openFile(uri: Uri, mimeType: String) {
        // The receiving app has no rights to our cache or to shared storage, so
        // the read grant has to travel with the intent.
        val view = Intent(Intent.ACTION_VIEW)
            .setDataAndType(uri, mimeType)
            .addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
        try {
            startActivity(view)
            return
        } catch (t: Throwable) {
            Log.w(TAG, "no app claimed $mimeType, offering a chooser", t)
        }
        // Nothing registered for that exact type: let the user pick.
        val chooser = Intent.createChooser(
            Intent(Intent.ACTION_VIEW)
                .setDataAndType(uri, "*/*")
                .addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION),
            "Open with",
        )
        startActivitySafely(chooser, "No app on this device can open that file")
    }

    override fun pickFileToUpload() {
        try {
            uploadPickerLauncher.launch(arrayOf("*/*"))
        } catch (t: Throwable) {
            Log.w(TAG, "could not open the document picker", t)
            Toast.makeText(this, "Could not open the file picker", Toast.LENGTH_LONG).show()
        }
    }

    override fun requestNotificationPermission() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            val launched = try {
                notificationPermissionLauncher.launch(Manifest.permission.POST_NOTIFICATIONS)
                true
            } catch (t: Throwable) {
                Log.w(TAG, "could not launch the notification permission dialog", t)
                false
            }
            if (launched) return
        }
        // Below API 33 there is no runtime permission (and on 33+ the dialog can be
        // permanently suppressed), so send the user to the settings screen instead.
        val intent = Intent(Settings.ACTION_APP_NOTIFICATION_SETTINGS)
            .putExtra(Settings.EXTRA_APP_PACKAGE, packageName)
        startActivitySafely(intent, "Could not open notification settings on this device")
    }

    override fun openAllFilesSettings() {
        val intent = try {
            Permissions.allFilesAccessIntent(this)
        } catch (t: Throwable) {
            Log.w(TAG, "could not build the all-files-access intent", t)
            return
        }
        startActivitySafely(
            intent,
            "Could not open the All files access settings on this device",
        )
    }

    override fun openBatteryExemptionSettings() {
        val intent = try {
            Permissions.batteryOptimizationIntent(this)
        } catch (t: Throwable) {
            Log.w(TAG, "could not build the battery optimisation intent", t)
            return
        }
        startActivitySafely(
            intent,
            "Could not open battery optimisation settings on this device",
        )
    }

    // ------------------------------------------------------------------ helpers

    /**
     * Some OEM ROMs ship without these settings screens, and an unhandled
     * ActivityNotFoundException would take the app down mid-onboarding.
     */
    private fun startActivitySafely(intent: Intent, failureMessage: String) {
        try {
            startActivity(intent)
        } catch (t: Throwable) {
            Log.w(TAG, "no activity found for ${intent.action}", t)
            Toast.makeText(this, failureMessage, Toast.LENGTH_LONG).show()
        }
    }
}
