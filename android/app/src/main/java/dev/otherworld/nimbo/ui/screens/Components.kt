/*
 * Components.kt — shared, stateless UI building blocks for every Nimbo screen,
 * plus the byte/speed formatting helpers used anywhere a size is shown.
 * Owned by the screens slice; no app state, no engine calls.
 */
package dev.otherworld.nimbo.ui.screens

import androidx.compose.material3.BadgedBox

import androidx.compose.material3.Badge

import androidx.compose.material.icons.filled.Notifications

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material.icons.filled.CloudDone
import androidx.compose.material.icons.filled.CloudOff
import androidx.compose.material.icons.filled.WarningAmber
import androidx.compose.material.icons.filled.Pause
import androidx.compose.material.icons.filled.Sync
import androidx.compose.material3.ElevatedCard
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.material3.TopAppBarDefaults
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp
import dev.otherworld.nimbo.core.SyncProgress
import java.util.Locale

/* ------------------------------------------------------------------ */
/* Formatting helpers                                                  */
/* ------------------------------------------------------------------ */

private val BINARY_UNITS = arrayOf("KB", "MB", "GB", "TB", "PB")

/** Human size, binary units, one decimal: 1_300_000 -> "1.2 MB". */
fun formatBytes(b: Long): String {
    if (b <= 0L) return "0 B"
    if (b < 1024L) return "$b B"
    var value = b.toDouble() / 1024.0
    var unit = 0
    while (value >= 1024.0 && unit < BINARY_UNITS.size - 1) {
        value /= 1024.0
        unit++
    }
    return String.format(Locale.US, "%.1f %s", value, BINARY_UNITS[unit])
}

/** Transfer rate, or "" when there is nothing moving. */
fun formatSpeed(bytesPerSec: Long): String =
    if (bytesPerSec <= 0L) "" else formatBytes(bytesPerSec) + "/s"

/* ------------------------------------------------------------------ */
/* Small shared pieces                                                 */
/* ------------------------------------------------------------------ */

/** Quiet uppercase-ish section label used above lists. */
@Composable
fun SectionHeader(text: String) {
    Text(
        text = text,
        style = MaterialTheme.typography.titleSmall,
        fontWeight = FontWeight.SemiBold,
        color = MaterialTheme.colorScheme.onSurfaceVariant,
        modifier = Modifier.padding(bottom = 4.dp),
    )
}

/** Centred "nothing here yet" block with an optional call to action. */
@Composable
fun EmptyState(
    icon: ImageVector,
    title: String,
    subtitle: String,
    modifier: Modifier = Modifier,
    action: @Composable (() -> Unit)? = null,
) {
    Column(
        modifier = modifier
            .fillMaxWidth()
            .padding(horizontal = 24.dp, vertical = 40.dp),
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        Box(
            modifier = Modifier
                .size(72.dp)
                .clip(CircleShape)
                .background(MaterialTheme.colorScheme.surfaceVariant),
            contentAlignment = Alignment.Center,
        ) {
            Icon(
                imageVector = icon,
                contentDescription = null,
                tint = MaterialTheme.colorScheme.onSurfaceVariant,
                modifier = Modifier.size(34.dp),
            )
        }
        Spacer(Modifier.height(20.dp))
        Text(
            text = title,
            style = MaterialTheme.typography.titleMedium,
            textAlign = TextAlign.Center,
        )
        Spacer(Modifier.height(8.dp))
        Text(
            text = subtitle,
            style = MaterialTheme.typography.bodyMedium,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
            textAlign = TextAlign.Center,
        )
        if (action != null) {
            Spacer(Modifier.height(24.dp))
            action()
        }
    }
}

/** Stacked label/value pair — copes with long URLs better than a single row. */
@Composable
fun LabelValueRow(label: String, value: String, modifier: Modifier = Modifier) {
    Column(
        modifier = modifier
            .fillMaxWidth()
            .padding(vertical = 10.dp),
    ) {
        Text(
            text = label,
            style = MaterialTheme.typography.labelMedium,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
        Spacer(Modifier.height(3.dp))
        Text(
            text = if (value.isBlank()) "—" else value,
            style = MaterialTheme.typography.bodyMedium,
        )
    }
}

/** Compact tinted pill, used for counts and yes/no facts. */
@Composable
fun StatPill(
    text: String,
    container: Color = MaterialTheme.colorScheme.secondaryContainer,
    content: Color = MaterialTheme.colorScheme.onSecondaryContainer,
) {
    Surface(
        shape = RoundedCornerShape(50),
        color = container,
        contentColor = content,
    ) {
        Text(
            text = text,
            style = MaterialTheme.typography.labelMedium,
            modifier = Modifier.padding(horizontal = 12.dp, vertical = 6.dp),
        )
    }
}

/** Coloured circle holding the first letter of [name] — stands in for remote icons. */
@Composable
fun InitialCircle(name: String, diameter: Dp = 56.dp) {
    val letter = name.trim().firstOrNull()?.uppercaseChar()?.toString() ?: "?"
    val palette = AVATAR_COLORS
    val index = ((name.hashCode() % palette.size) + palette.size) % palette.size
    Box(
        modifier = Modifier
            .size(diameter)
            .clip(CircleShape)
            .background(palette[index]),
        contentAlignment = Alignment.Center,
    ) {
        Text(
            text = letter,
            style = MaterialTheme.typography.titleLarge,
            fontWeight = FontWeight.SemiBold,
            color = Color.White,
        )
    }
}

private val AVATAR_COLORS = listOf(
    Color(0xFF0082C9),
    Color(0xFF00695C),
    Color(0xFF4527A0),
    Color(0xFFC2185B),
    Color(0xFFEF6C00),
    Color(0xFF2E7D32),
    Color(0xFF00838F),
    Color(0xFF5D4037),
)

/** One top bar shape for the whole app: optional back arrow, optional actions. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun NimboTopBar(
    title: String,
    onBack: (() -> Unit)? = null,
    actions: @Composable androidx.compose.foundation.layout.RowScope.() -> Unit = {},
) {
    TopAppBar(
        title = {
            Text(
                text = title,
                style = MaterialTheme.typography.titleLarge,
                fontWeight = FontWeight.SemiBold,
            )
        },
        navigationIcon = {
            if (onBack != null) {
                IconButton(onClick = onBack) {
                    Icon(Icons.AutoMirrored.Filled.ArrowBack, contentDescription = "Back")
                }
            }
        },
        actions = actions,
        colors = TopAppBarDefaults.topAppBarColors(
            containerColor = MaterialTheme.colorScheme.surface,
            titleContentColor = MaterialTheme.colorScheme.onSurface,
        ),
    )
}

/**
 * The notification bell, with its count.
 *
 * Lives in every tab's top bar rather than only in Files: notifications belong
 * to the account, not to a screen, and a badge you can only see from one tab is
 * a badge you will miss.
 *
 * The badge appears only when there is something to say — a "0" is noise that
 * teaches people to stop looking.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun NotificationBell(count: Int, onClick: () -> Unit) {
    BadgedBox(
        badge = {
            if (count > 0) {
                Badge { Text(if (count > 99) "99+" else "$count") }
            }
        },
    ) {
        IconButton(onClick = onClick) {
            Icon(
                Icons.Filled.Notifications,
                contentDescription = if (count > 0) "Notifications, $count new" else "Notifications",
            )
        }
    }
}

/* ------------------------------------------------------------------ */
/* Status card                                                         */
/* ------------------------------------------------------------------ */

/**
 * The headline card on Home: what the engine is doing right now, with a
 * determinate bar while transferring and an indeterminate one while scanning.
 */
@Composable
fun StatusCard(
    status: String,
    progress: SyncProgress?,
    paused: Boolean,
    running: Boolean,
    modifier: Modifier = Modifier,
    /**
     * Set when a folder has stopped syncing. Outranks every other state: the
     * engine's status is account-wide, so it can say "Up to date" while a folder
     * is broken, and showing that would be a lie.
     */
    problem: String? = null,
) {
    val scheme = MaterialTheme.colorScheme
    val active = progress != null && progress.active

    val icon: ImageVector
    val tint: Color
    val bubble: Color
    val headline: String
    when {
        problem != null -> {
            icon = Icons.Filled.WarningAmber
            tint = scheme.error
            bubble = scheme.errorContainer
            headline = problem
        }
        !running -> {
            icon = Icons.Filled.CloudOff
            tint = scheme.onSurfaceVariant
            bubble = scheme.surfaceVariant
            headline = "Not running"
        }
        paused -> {
            icon = Icons.Filled.Pause
            tint = scheme.onTertiaryContainer
            bubble = scheme.tertiaryContainer
            headline = "Paused"
        }
        active -> {
            icon = Icons.Filled.Sync
            tint = scheme.onPrimaryContainer
            bubble = scheme.primaryContainer
            headline = if (progress!!.enumerating) "Scanning…" else "Syncing"
        }
        else -> {
            icon = Icons.Filled.CloudDone
            tint = scheme.onPrimaryContainer
            bubble = scheme.primaryContainer
            headline = "Up to date"
        }
    }

    val fallbackSub = when {
        !running -> "Sync service is stopped"
        paused -> "Tap Resume to continue syncing"
        active -> "Working…"
        else -> "Watching your folders for changes"
    }

    ElevatedCard(modifier = modifier.fillMaxWidth()) {
        Column(modifier = Modifier.padding(20.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Box(
                    modifier = Modifier
                        .size(46.dp)
                        .clip(CircleShape)
                        .background(bubble),
                    contentAlignment = Alignment.Center,
                ) {
                    Icon(
                        imageVector = icon,
                        contentDescription = null,
                        tint = tint,
                        modifier = Modifier.size(24.dp),
                    )
                }
                Spacer(Modifier.width(14.dp))
                Column(modifier = Modifier.weight(1f)) {
                    Text(
                        text = headline,
                        style = MaterialTheme.typography.titleMedium,
                        fontWeight = FontWeight.SemiBold,
                    )
                    Spacer(Modifier.height(2.dp))
                    Text(
                        text = if (status.isBlank()) fallbackSub else status,
                        style = MaterialTheme.typography.bodySmall,
                        color = scheme.onSurfaceVariant,
                        maxLines = 2,
                        overflow = TextOverflow.Ellipsis,
                    )
                }
            }

            if (active && progress != null) {
                Spacer(Modifier.height(18.dp))
                if (progress.enumerating || progress.total <= 0L) {
                    LinearProgressIndicator(
                        modifier = Modifier
                            .fillMaxWidth()
                            .height(6.dp)
                            .clip(RoundedCornerShape(3.dp)),
                    )
                } else {
                    val fraction = (progress.done.toFloat() / progress.total.toFloat())
                        .coerceIn(0f, 1f)
                    LinearProgressIndicator(
                        progress = { fraction },
                        modifier = Modifier
                            .fillMaxWidth()
                            .height(6.dp)
                            .clip(RoundedCornerShape(3.dp)),
                    )
                }

                if (progress.current.isNotBlank()) {
                    Spacer(Modifier.height(12.dp))
                    Text(
                        text = progress.current,
                        style = MaterialTheme.typography.bodySmall,
                        color = scheme.onSurface,
                        maxLines = 1,
                        overflow = TextOverflow.Ellipsis,
                    )
                }

                Spacer(Modifier.height(8.dp))
                Row(
                    modifier = Modifier.fillMaxWidth(),
                    horizontalArrangement = Arrangement.SpaceBetween,
                    verticalAlignment = Alignment.CenterVertically,
                ) {
                    val left = buildString {
                        if (progress.total > 0L) {
                            append("${progress.done} of ${progress.total} files")
                        } else if (progress.enumerating) {
                            append("Looking for changes")
                        }
                        if (progress.totalBytes > 0L) {
                            if (isNotEmpty()) append("  ·  ")
                            append(formatBytes(progress.doneBytes))
                            append(" / ")
                            append(formatBytes(progress.totalBytes))
                        }
                    }
                    Text(
                        text = left,
                        style = MaterialTheme.typography.labelMedium,
                        color = scheme.onSurfaceVariant,
                        maxLines = 1,
                        overflow = TextOverflow.Ellipsis,
                        modifier = Modifier.weight(1f, fill = false),
                    )
                    val speed = formatSpeed(progress.speed)
                    if (speed.isNotEmpty()) {
                        Spacer(Modifier.width(8.dp))
                        Text(
                            text = speed,
                            style = MaterialTheme.typography.labelMedium,
                            fontWeight = FontWeight.SemiBold,
                            color = scheme.primary,
                            maxLines = 1,
                        )
                    }
                }
            }
        }
    }
}

/**
 * An error shown INSIDE a bottom sheet.
 *
 * Snackbars are hosted by the Scaffold and render behind a ModalBottomSheet, so
 * a failure reported that way while a sheet is open is invisible: the user taps
 * a button, nothing happens, and they reasonably conclude it worked.
 */
@Composable
fun SheetError(message: String?, modifier: Modifier = Modifier) {
    if (message == null) return
    Row(
        modifier = modifier
            .fillMaxWidth()
            .padding(horizontal = 24.dp, vertical = 10.dp),
        verticalAlignment = Alignment.Top,
    ) {
        Icon(
            imageVector = Icons.Filled.WarningAmber,
            contentDescription = null,
            tint = MaterialTheme.colorScheme.error,
            modifier = Modifier.size(18.dp),
        )
        Spacer(Modifier.width(10.dp))
        Text(
            text = message,
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.error,
        )
    }
}
