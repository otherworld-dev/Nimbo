/*
 * SupporterPrefs.kt — the supporter state that lives on this phone: the
 * cached status and the user's choices. Not secret (keys live in
 * KeystoreSecretStore), so plain SharedPreferences. Reads never throw.
 */
package dev.otherworld.nimbo.supporter

import android.content.Context
import android.content.SharedPreferences
import android.util.Log
import androidx.core.content.edit
import dev.otherworld.nimbo.BuildConfig

class SupporterPrefs(context: Context) : StatusCache {

    private val prefs: SharedPreferences =
        context.applicationContext.getSharedPreferences(PREFS, Context.MODE_PRIVATE)

    override fun read(): SupporterStatus =
        SupporterStatus.decode(runCatching { prefs.getString(KEY_STATUS, null) }.getOrNull())

    override fun write(status: SupporterStatus) = save { putString(KEY_STATUS, status.encode()) }

    var icon: AppIcon
        get() = AppIcon.fromStored(string(KEY_ICON))
        set(value) = save { putString(KEY_ICON, value.name) }

    var accent: AccentChoice
        get() = AccentChoice.fromStored(string(KEY_ACCENT))
        set(value) = save { putString(KEY_ACCENT, value.name) }

    var firstSeenAt: Long
        get() = runCatching { prefs.getLong(KEY_FIRST_SEEN, 0L) }.getOrDefault(0L)
        set(value) = save { putLong(KEY_FIRST_SEEN, value) }

    var nudgeRetired: Boolean
        get() = runCatching { prefs.getBoolean(KEY_NUDGE_RETIRED, false) }.getOrDefault(false)
        set(value) = save { putBoolean(KEY_NUDGE_RETIRED, value) }

    var debugOverride: SupporterTier?
        get() = string(KEY_DEBUG)?.let { name -> SupporterTier.entries.firstOrNull { it.name == name } }
        set(value) = save { if (value == null) remove(KEY_DEBUG) else putString(KEY_DEBUG, value.name) }

    fun choices() = SupporterChoices(
        icon = icon,
        accent = accent,
        firstSeenAt = firstSeenAt,
        nudgeRetired = nudgeRetired,
        // A release build ignores a stored override, even one left behind by
        // a debug build installed over the same data.
        debugOverride = if (BuildConfig.DEBUG) debugOverride else null,
    )

    private fun string(key: String): String? = runCatching { prefs.getString(key, null) }.getOrNull()

    private fun save(block: SharedPreferences.Editor.() -> Unit) {
        runCatching { prefs.edit(action = block) }
            .onFailure { Log.w(TAG, "could not save a supporter preference", it) }
    }

    private companion object {
        const val TAG = "NimboSupporter"
        const val PREFS = "supporter_prefs"
        const val KEY_STATUS = "status"
        const val KEY_ICON = "icon"
        const val KEY_ACCENT = "accent"
        const val KEY_FIRST_SEEN = "first_seen_at"
        const val KEY_NUDGE_RETIRED = "nudge_retired"
        const val KEY_DEBUG = "debug_tier"
    }
}
