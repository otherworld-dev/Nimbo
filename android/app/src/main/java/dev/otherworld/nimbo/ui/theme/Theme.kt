/*
 * Theme.kt — the Material 3 theme for the whole app.
 *
 * Wallpaper-based dynamic colour is deliberately off. The accent instead comes
 * from the user's own Nextcloud theme, exactly as the desktop client does — the
 * app should look like their server, not like their wallpaper. The schemes below
 * are the fallback for before sign-in, offline, or a server colour we cannot
 * read.
 */
package dev.otherworld.nimbo.ui.theme

import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Typography
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.compositeOver

/** Nimbo indigo, the desktop app's brand accent (#5856E0). */
val NimboIndigo = Color(0xFF5856E0)

private val LightColors = lightColorScheme(
    primary = NimboIndigo,
    onPrimary = Color(0xFFFFFFFF),
    primaryContainer = Color(0xFFE2DFFF),
    onPrimaryContainer = Color(0xFF0B006B),
    secondary = Color(0xFF5D5C71),
    onSecondary = Color(0xFFFFFFFF),
    secondaryContainer = Color(0xFFE2E0F9),
    onSecondaryContainer = Color(0xFF1A1A2C),
    tertiary = Color(0xFF795369),
    onTertiary = Color(0xFFFFFFFF),
    tertiaryContainer = Color(0xFFFFD8EB),
    onTertiaryContainer = Color(0xFF2F1124),
    error = Color(0xFFBA1A1A),
    onError = Color(0xFFFFFFFF),
    errorContainer = Color(0xFFFFDAD6),
    onErrorContainer = Color(0xFF410002),
    background = Color(0xFFFFFBFF),
    onBackground = Color(0xFF1C1B1F),
    surface = Color(0xFFFFFBFF),
    onSurface = Color(0xFF1C1B1F),
    surfaceVariant = Color(0xFFE4E1EC),
    onSurfaceVariant = Color(0xFF47464F),
    outline = Color(0xFF777680),
    outlineVariant = Color(0xFFC8C5D0),
    inverseSurface = Color(0xFF313034),
    inverseOnSurface = Color(0xFFF3EFF4),
    inversePrimary = Color(0xFFC2C1FF),
    scrim = Color(0xFF000000),
)

private val DarkColors = darkColorScheme(
    primary = Color(0xFFC2C1FF),
    onPrimary = Color(0xFF1800A7),
    primaryContainer = Color(0xFF332DBC),
    onPrimaryContainer = Color(0xFFE2DFFF),
    secondary = Color(0xFFC6C4DD),
    onSecondary = Color(0xFF2F2F42),
    secondaryContainer = Color(0xFF454559),
    onSecondaryContainer = Color(0xFFE2E0F9),
    tertiary = Color(0xFFE9B9D2),
    onTertiary = Color(0xFF47263A),
    tertiaryContainer = Color(0xFF5F3C51),
    onTertiaryContainer = Color(0xFFFFD8EB),
    error = Color(0xFFFFB4AB),
    onError = Color(0xFF690005),
    errorContainer = Color(0xFF93000A),
    onErrorContainer = Color(0xFFFFB4AB),
    background = Color(0xFF1C1B1F),
    onBackground = Color(0xFFE5E1E6),
    surface = Color(0xFF1C1B1F),
    onSurface = Color(0xFFE5E1E6),
    surfaceVariant = Color(0xFF47464F),
    onSurfaceVariant = Color(0xFFC8C5D0),
    outline = Color(0xFF918F9A),
    outlineVariant = Color(0xFF47464F),
    inverseSurface = Color(0xFFE5E1E6),
    inverseOnSurface = Color(0xFF313034),
    inversePrimary = NimboIndigo,
    scrim = Color(0xFF000000),
)

/** Material 3 default type scale — no custom fonts in the walking skeleton. */
private val NimboTypography = Typography()

@Composable
fun NimboTheme(
    darkTheme: Boolean = isSystemInDarkTheme(),
    /**
     * The user's Nextcloud theme colour, as the desktop client uses it. Null —
     * before sign-in, offline, or when the server reports something unusable —
     * keeps the palette below, which is never wrong, only generic.
     */
    accent: Color? = null,
    content: @Composable () -> Unit,
) {
    val base = if (darkTheme) DarkColors else LightColors
    // Only the primary roles are re-pointed. Deriving a whole palette from one
    // hex needs the HCT maths in material-color-utilities, which this project's
    // pinned dependency set does not include — and the desktop does the same
    // thing anyway: it emits ONE accent, not a generated scheme.
    val scheme = if (accent == null) base else base.copy(
        primary = accent,
        onPrimary = readableOn(accent),
        // The container is the same hue softened toward the surface, so it
        // still reads as "this app's colour" without competing with the accent.
        primaryContainer = accent.copy(alpha = if (darkTheme) 0.32f else 0.18f)
            .compositeOver(base.surface),
        onPrimaryContainer = base.onSurface,
    )
    MaterialTheme(
        colorScheme = scheme,
        typography = NimboTypography,
        content = content,
    )
}
