/*
 * NotificationsScreen.kt — what the server has been trying to tell you.
 *
 * The engine has always known about these; until now nothing showed them. Each
 * row keeps whatever buttons the notification arrived with, because only the
 * app that sent it knows what "Accept" ought to do.
 */
package dev.otherworld.nimbo.ui.screens

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Close
import androidx.compose.material.icons.filled.DoneAll
import androidx.compose.material.icons.filled.NotificationsNone
import androidx.compose.material.icons.filled.Refresh
import androidx.compose.material.icons.filled.WarningAmber
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.animation.animateColorAsState
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.ui.graphics.Color
import androidx.compose.runtime.snapshotFlow
import kotlinx.coroutines.flow.dropWhile
import kotlinx.coroutines.flow.filter
import kotlinx.coroutines.flow.first
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import dev.otherworld.nimbo.core.NcNotification
import dev.otherworld.nimbo.core.NcNotificationAction

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun NotificationsScreen(
    items: List<NcNotification>,
    loading: Boolean,
    error: String?,
    busy: Boolean,
    focusedId: Int?,
    onFocusShown: () -> Unit,
    onRunAction: (NcNotificationAction) -> Unit,
    onOpenLink: (NcNotification) -> Unit,
    onDismiss: (NcNotification) -> Unit,
    onDismissAll: () -> Unit,
    onRefresh: () -> Unit,
    onBack: () -> Unit,
) {
    var confirmingClearAll by remember { mutableStateOf(false) }
    val listState = rememberLazyListState()

    // Arriving from the shade: scroll to the notification that was tapped and
    // mark it, so the user lands on the thing they tapped rather than on a list
    // they then have to search. Runs once the list has actually loaded.
    LaunchedEffect(focusedId, items) {
        val id = focusedId ?: return@LaunchedEffect
        val index = items.indexOfFirst { it.id == id }
        if (index < 0) {
            // Not there YET. The screen composes with the previously loaded list
            // while the fresh one is still being fetched, and a notification that
            // arrived since will be missing from it. Giving up here cleared the
            // focus before the real list landed — which is every case where a
            // notification arrives after the screen has been opened once, i.e.
            // the normal one. Wait instead: this effect re-runs when items change.
            return@LaunchedEffect
        }
        runCatching { listState.animateScrollToItem(index) }
        // Held until the user actually does something, rather than for a fixed
        // few seconds: the timer would start after a cold launch AND a network
        // fetch, so it could easily expire before they had looked at the screen.
        // The programmatic scroll above has finished by here, so the next scroll
        // is theirs.
        // dropWhile guards against the scroll above still reading as in-progress
        // for a frame, which would count as the user's scroll and clear the
        // highlight before it had been drawn.
        snapshotFlow { listState.isScrollInProgress }.dropWhile { it }.filter { it }.first()
        onFocusShown()
    }

    Scaffold(
        modifier = Modifier.fillMaxSize(),
        topBar = {
            Column {
                NimboTopBar(title = "Notifications", onBack = onBack) {
                    if (items.isNotEmpty()) {
                        IconButton(onClick = { confirmingClearAll = true }, enabled = !busy) {
                            Icon(Icons.Filled.DoneAll, contentDescription = "Clear all")
                        }
                    }
                    IconButton(onClick = onRefresh) {
                        Icon(Icons.Filled.Refresh, contentDescription = "Refresh")
                    }
                }
                if (busy || loading) LinearProgressIndicator(modifier = Modifier.fillMaxWidth())
            }
        },
    ) { inner ->
        Box(
            modifier = Modifier
                .fillMaxSize()
                .padding(inner),
        ) {
            when {
                loading && items.isEmpty() -> Box(
                    modifier = Modifier.fillMaxSize(),
                    contentAlignment = Alignment.Center,
                ) { CircularProgressIndicator() }

                error != null -> EmptyState(
                    icon = Icons.Filled.WarningAmber,
                    title = "Couldn't load notifications",
                    subtitle = error,
                )

                items.isEmpty() -> EmptyState(
                    icon = Icons.Filled.NotificationsNone,
                    title = "Nothing new",
                    subtitle = "Shares, mentions and server messages show up here.",
                )

                else -> LazyColumn(
                    state = listState,
                    modifier = Modifier.fillMaxSize(),
                    contentPadding = PaddingValues(bottom = 24.dp),
                ) {
                    items(items = items, key = { it.id }) { item ->
                        NotificationRow(
                            item = item,
                            enabled = !busy,
                            highlighted = item.id == focusedId,
                            onRunAction = onRunAction,
                            onOpen = { onOpenLink(item) },
                            onDismiss = { onDismiss(item) },
                        )
                        HorizontalDivider()
                    }
                }
            }
        }
    }

    if (confirmingClearAll) {
        AlertDialog(
            onDismissRequest = { confirmingClearAll = false },
            title = { Text("Clear all notifications?") },
            text = {
                Column {
                    Text("All ${items.size} will be cleared.")
                    Spacer(Modifier.height(8.dp))
                    // Both facts matter and neither is obvious from the button.
                    Text(
                        "They're cleared on the server, so they go from your other " +
                            "devices too. Dismissed notifications aren't kept anywhere.",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            },
            confirmButton = {
                TextButton(onClick = { confirmingClearAll = false; onDismissAll() }) {
                    Text("Clear all")
                }
            },
            dismissButton = {
                TextButton(onClick = { confirmingClearAll = false }) { Text("Cancel") }
            },
        )
    }
}

@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun NotificationRow(
    item: NcNotification,
    enabled: Boolean,
    highlighted: Boolean,
    onRunAction: (NcNotificationAction) -> Unit,
    onOpen: () -> Unit,
    onDismiss: () -> Unit,
) {
    // A tint rather than a border or a scale: it says "this one" without
    // implying the row is selected or that anything is required of it.
    val background by animateColorAsState(
        targetValue = if (highlighted) {
            MaterialTheme.colorScheme.primary.copy(alpha = 0.12f)
        } else {
            Color.Transparent
        },
        label = "notificationHighlight",
    )
    Column(
        modifier = Modifier
            .fillMaxWidth()
            .background(background)
            .then(if (item.link.isNotBlank()) Modifier.clickable(onClick = onOpen) else Modifier)
            .padding(start = 16.dp, end = 4.dp, top = 12.dp, bottom = 12.dp),
    ) {
        Row(verticalAlignment = Alignment.Top) {
            Box(
                modifier = Modifier
                    .size(36.dp)
                    .clip(RoundedCornerShape(9.dp))
                    .background(MaterialTheme.colorScheme.surfaceVariant),
                contentAlignment = Alignment.Center,
            ) {
                InitialCircle(name = item.app.ifBlank { "Nextcloud" }, diameter = 36.dp)
            }
            Spacer(Modifier.width(14.dp))
            Column(modifier = Modifier.weight(1f)) {
                Text(
                    text = item.subject.ifBlank { "Notification" },
                    style = MaterialTheme.typography.bodyLarge,
                )
                if (item.message.isNotBlank()) {
                    Text(
                        text = item.message,
                        style = MaterialTheme.typography.bodyMedium,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        maxLines = 3,
                        overflow = TextOverflow.Ellipsis,
                    )
                }
                if (item.whenLabel.isNotEmpty()) {
                    Spacer(Modifier.height(2.dp))
                    Text(
                        text = item.whenLabel,
                        style = MaterialTheme.typography.labelSmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            }
            IconButton(onClick = onDismiss, enabled = enabled) {
                Icon(
                    Icons.Filled.Close,
                    contentDescription = "Dismiss: ${item.subject}",
                    tint = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
        }

        // The server's own buttons, with the server's own labels. Nimbo has no
        // idea what any given app means by them and does not pretend to.
        if (item.actions.isNotEmpty()) {
            Spacer(Modifier.height(10.dp))
            // Every action, wrapping. take(N) would silently drop the rest, and
            // a dropped "Decline" is not a cosmetic loss; a fixed Row would push
            // the later buttons off the side of a narrow screen instead.
            FlowRow(
                modifier = Modifier.padding(start = 50.dp, end = 8.dp),
                horizontalArrangement = Arrangement.spacedBy(8.dp),
                verticalArrangement = Arrangement.spacedBy(4.dp),
            ) {
                item.actions.forEach { action ->
                    if (action.primary) {
                        Button(onClick = { onRunAction(action) }, enabled = enabled) {
                            Text(action.label)
                        }
                    } else {
                        OutlinedButton(onClick = { onRunAction(action) }, enabled = enabled) {
                            Text(action.label)
                        }
                    }
                }
            }
        }
    }
}
