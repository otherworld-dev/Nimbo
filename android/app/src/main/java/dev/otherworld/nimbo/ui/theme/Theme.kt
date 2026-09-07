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

/** Nextcloud brand blue. */
val NimboBlue = Color(0xFF0082C9)

private val LightColors = lightColorScheme(
    primary = NimboBlue,
    onPrimary = Color(0xFFFFFFFF),
    primaryContainer = Color(0xFFCDE5FF),
    onPrimaryContainer = Color(0xFF001D33),
    secondary = Color(0xFF51606F),
    onSecondary = Color(0xFFFFFFFF),
    secondaryContainer = Color(0xFFD4E4F6),
    onSecondaryContainer = Color(0xFF0D1D2A),
    tertiary = Color(0xFF00696E),
    onTertiary = Color(0xFFFFFFFF),
    tertiaryContainer = Color(0xFF6FF6FF),
    onTertiaryContainer = Color(0xFF002022),
    error = Color(0xFFBA1A1A),
    onError = Color(0xFFFFFFFF),
    errorContainer = Color(0xFFFFDAD6),
    onErrorContainer = Color(0xFF410002),
    background = Color(0xFFFCFCFF),
    onBackground = Color(0xFF1A1C1E),
    surface = Color(0xFFFCFCFF),
    onSurface = Color(0xFF1A1C1E),
    surfaceVariant = Color(0xFFDDE3EA),
    onSurfaceVariant = Color(0xFF41484D),
    outline = Color(0xFF71787E),
    outlineVariant = Color(0xFFC1C7CE),
    inverseSurface = Color(0xFF2F3133),
    inverseOnSurface = Color(0xFFF0F0F3),
    inversePrimary = Color(0xFF95CCFF),
    scrim = Color(0xFF000000),
)

private val DarkColors = darkColorScheme(
    primary = Color(0xFF95CCFF),
    onPrimary = Color(0xFF003352),
    primaryContainer = Color(0xFF004A75),
    onPrimaryContainer = Color(0xFFCDE5FF),
    secondary = Color(0xFFB8C8DA),
    onSecondary = Color(0xFF233240),
    secondaryContainer = Color(0xFF394857),
    onSecondaryContainer = Color(0xFFD4E4F6),
    tertiary = Color(0xFF4CD9E2),
    onTertiary = Color(0xFF00373A),
    tertiaryContainer = Color(0xFF004F53),
    onTertiaryContainer = Color(0xFF6FF6FF),
    error = Color(0xFFFFB4AB),
    onError = Color(0xFF690005),
    errorContainer = Color(0xFF93000A),
    onErrorContainer = Color(0xFFFFDAD6),
    background = Color(0xFF1A1C1E),
    onBackground = Color(0xFFE2E2E5),
    surface = Color(0xFF1A1C1E),
    onSurface = Color(0xFFE2E2E5),
    surfaceVariant = Color(0xFF41484D),
    onSurfaceVariant = Color(0xFFC1C7CE),
    outline = Color(0xFF8B9198),
    outlineVariant = Color(0xFF41484D),
    inverseSurface = Color(0xFFE2E2E5),
    inverseOnSurface = Color(0xFF2F3133),
    inversePrimary = NimboBlue,
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
