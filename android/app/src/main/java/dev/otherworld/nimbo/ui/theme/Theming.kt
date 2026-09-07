/*
 * Theming.kt — the decisions behind the theme, kept pure.
 *
 * All three are made from values someone else controls: a colour a server admin
 * typed into Nextcloud, an appearance the user set there, and a preference they
 * set here. None of them can be trusted to be sensible, and none of them should
 * be able to produce an app you cannot read.
 */
package dev.otherworld.nimbo.ui.theme

import androidx.compose.ui.graphics.Color

/** Where the app takes dark/light from. */
enum class AppearancePreference(val stored: String, val label: String) {
    /** The user's Nextcloud appearance, falling back to the phone. */
    FOLLOW_NEXTCLOUD("nextcloud", "Follow Nextcloud"),
    FOLLOW_SYSTEM("system", "Follow phone"),
    ALWAYS_DARK("dark", "Always dark"),
    ALWAYS_LIGHT("light", "Always light");

    companion object {
        /**
         * Reads a stored value. Anything unrecognised — a preference written by
         * a later version, a corrupted file — falls back to the default rather
         * than throwing on a value the user cannot see or fix.
         */
        fun fromStored(value: String?): AppearancePreference =
            entries.firstOrNull { it.stored == value } ?: FOLLOW_NEXTCLOUD
    }
}

/**
 * The server's theming colour, or null if it did not give us one we can use.
 *
 * Null matters: the caller keeps its own palette rather than substituting a
 * guess. A wrong accent is worse than no accent, because it can be illegible.
 */
fun parseThemeColor(hex: String?): Color? {
    val raw = hex?.trim()?.removePrefix("#") ?: return null
    val expanded = when (raw.length) {
        // #RGB shorthand, which Nextcloud accepts from an admin.
        3 -> raw.map { "$it$it" }.joinToString("")
        6 -> raw
        else -> return null
    }
    if (!expanded.all { it.isDigit() || it.lowercaseChar() in 'a'..'f' }) return null
    val value = expanded.toLongOrNull(16) ?: return null
    return Color(0xFF000000L or value)
}

/**
 * Black or white — whichever can be read on [background].
 *
 * Weighted for perceived luminance rather than averaged: pure green is far
 * lighter to the eye than pure blue, and an average would put white text on a
 * bright green accent.
 */
fun readableOn(background: Color): Color {
    val luminance = 0.2126 * background.red + 0.7152 * background.green + 0.0722 * background.blue
    return if (luminance > 0.5) Color.Black else Color.White
}

/**
 * Whether to render dark.
 *
 * [serverAppearance] is what Nextcloud reports: "dark", "light", or "default"
 * (meaning the user told Nextcloud to follow their own OS). "default", unknown
 * values and no answer at all are the same thing here — the server has no
 * opinion, so the phone decides.
 */
fun resolveDark(
    preference: AppearancePreference,
    serverAppearance: String?,
    systemDark: Boolean,
): Boolean = when (preference) {
    AppearancePreference.ALWAYS_DARK -> true
    AppearancePreference.ALWAYS_LIGHT -> false
    AppearancePreference.FOLLOW_SYSTEM -> systemDark
    AppearancePreference.FOLLOW_NEXTCLOUD -> when (serverAppearance?.trim()?.lowercase()) {
        "dark" -> true
        "light" -> false
        else -> systemDark
    }
}
