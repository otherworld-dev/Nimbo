/*
 * HomeScreen.kt — the main stateless screen: engine status, sync controls,
 * server quota, the list of synced folders and the last sync summary.
 *
 * Note on removal: the pinned signature exposes `onRemoveFolder: (String) -> Unit`
 * only, so the confirm dialog cannot pass an "also delete local copy" choice
 * through it. The optional `onRemoveFolderWithLocal` parameter (appended last, so
 * the pinned call sites are unaffected) enables that checkbox when the caller
 * wires it; when it is null the dialog states plainly that local files are kept.
 */
package dev.otherworld.nimbo.ui.screens

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
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
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Add
import androidx.compose.material.icons.filled.Apps
import androidx.compose.material.icons.filled.CloudQueue
import androidx.compose.material.icons.filled.Folder
import androidx.compose.material.icons.filled.MoreVert
import androidx.compose.material.icons.filled.Pause
import androidx.compose.material.icons.filled.PlayArrow
import androidx.compose.material.icons.filled.Storage
import androidx.compose.material.icons.filled.Sync
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.Checkbox
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.ExtendedFloatingActionButton
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.draw.clip
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material.icons.filled.WarningAmber
import dev.otherworld.nimbo.core.FailingFolder
import dev.otherworld.nimbo.core.failingSummary
import dev.otherworld.nimbo.core.isEverythingHealthy
import dev.otherworld.nimbo.core.FrozenFolder
import dev.otherworld.nimbo.core.PairSyncedEvent
import dev.otherworld.nimbo.core.Quota
import dev.otherworld.nimbo.core.SyncPair
import dev.otherworld.nimbo.core.SyncProgress

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun HomeScreen(
    status: String,
    progress: SyncProgress?,
    paused: Boolean,
    running: Boolean,
    pairs: List<SyncPair>,
    frozen: List<FrozenFolder>,
    failing: List<FailingFolder>,
    quota: Quota?,
    lastSynced: PairSyncedEvent?,
    lastSyncAt: Long,
    busy: Boolean,
    onSyncNow: () -> Unit,
    onTogglePause: () -> Unit,
    onAddFolder: () -> Unit,
    onRemoveFolder: (String) -> Unit,
    onOpenApps: () -> Unit,
    onOpenSettings: () -> Unit,
    onOpenDiagnostics: () -> Unit,
    onOpenNotifications: () -> Unit,
    notificationCount: Int,
    onSignOut: () -> Unit,
    onResumeFrozen: (String) -> Unit,
    onRemoveFolderWithLocal: ((String, Boolean) -> Unit)? = null,
) {
    var overflowOpen by remember { mutableStateOf(false) }
    var pendingRemoval by remember { mutableStateOf<String?>(null) }
    var deleteLocalCopy by remember { mutableStateOf(false) }

    Scaffold(
        modifier = Modifier.fillMaxSize(),
        topBar = {
            Column {
                NimboTopBar(title = "Nimbo") {
                    NotificationBell(notificationCount, onOpenNotifications)
                    IconButton(onClick = onOpenApps) {
                        Icon(Icons.Filled.Apps, contentDescription = "Nextcloud apps")
                    }
                    Box {
                        IconButton(onClick = { overflowOpen = true }) {
                            Icon(Icons.Filled.MoreVert, contentDescription = "More")
                        }
                        DropdownMenu(
                            expanded = overflowOpen,
                            onDismissRequest = { overflowOpen = false },
                        ) {
                            DropdownMenuItem(
                                text = { Text("Nextcloud apps") },
                                onClick = { overflowOpen = false; onOpenApps() },
                            )
                            DropdownMenuItem(
                                text = { Text("Settings") },
                                onClick = { overflowOpen = false; onOpenSettings() },
                            )
                            DropdownMenuItem(
                                text = { Text("Diagnostics") },
                                onClick = { overflowOpen = false; onOpenDiagnostics() },
                            )
                            HorizontalDivider()
                            DropdownMenuItem(
                                text = { Text("Sign out") },
                                onClick = { overflowOpen = false; onSignOut() },
                            )
                        }
                    }
                }
                if (busy) {
                    LinearProgressIndicator(modifier = Modifier.fillMaxWidth())
                }
            }
        },
        floatingActionButton = {
            // Always present, including with no pairs. The empty state's own
            // button sits below the fold on a short screen once the bottom nav
            // takes its share, and nothing hints that the page scrolls — which
            // left a first-run user with no visible way to add their first folder.
            ExtendedFloatingActionButton(
                onClick = onAddFolder,
                icon = { Icon(Icons.Filled.Add, contentDescription = null) },
                text = { Text("Add folder") },
            )
        },
    ) { inner ->
        LazyColumn(
            modifier = Modifier
                .fillMaxSize()
                .padding(inner),
            contentPadding = PaddingValues(
                start = 20.dp,
                end = 20.dp,
                top = 16.dp,
                bottom = 104.dp,
            ),
            verticalArrangement = Arrangement.spacedBy(16.dp),
        ) {
            item {
                StatusCard(
                    status = status,
                    progress = progress,
                    paused = paused,
                    running = running,
                    // The engine's status is account-wide; never let it claim all
                    // is well while a folder has stopped syncing.
                    problem = if (isEverythingHealthy(status, failing)) null
                    else failingSummary(failing),
                )
            }

            // The detail behind the headline, per folder, with the engine's own
            // words for why.
            items(items = failing, key = { "failing-" + it.localDir }) { folder ->
                FailingFolderCard(folder)
            }

            // Above the controls on purpose: a paused folder is not syncing, and
            // burying that under the buttons that imply it is would be a lie.
            items(items = frozen, key = { "frozen-" + it.localDir }) { folder ->
                FrozenFolderCard(folder = folder, busy = busy, onResume = onResumeFrozen)
            }

            item {
                Row(
                    modifier = Modifier.fillMaxWidth(),
                    horizontalArrangement = Arrangement.spacedBy(12.dp),
                ) {
                    Button(
                        onClick = onSyncNow,
                        enabled = !busy,
                        shape = RoundedCornerShape(14.dp),
                        modifier = Modifier
                            .weight(1f)
                            .height(48.dp),
                    ) {
                        Icon(
                            Icons.Filled.Sync,
                            contentDescription = null,
                            modifier = Modifier.size(18.dp),
                        )
                        Spacer(Modifier.width(8.dp))
                        Text("Sync now")
                    }
                    OutlinedButton(
                        onClick = onTogglePause,
                        enabled = !busy,
                        shape = RoundedCornerShape(14.dp),
                        modifier = Modifier
                            .weight(1f)
                            .height(48.dp),
                    ) {
                        Icon(
                            imageVector = if (paused) Icons.Filled.PlayArrow else Icons.Filled.Pause,
                            contentDescription = null,
                            modifier = Modifier.size(18.dp),
                        )
                        Spacer(Modifier.width(8.dp))
                        Text(if (paused) "Resume" else "Pause")
                    }
                }
            }

            if (quota != null) {
                item { QuotaCard(quota) }
            }

            item {
                Row(
                    modifier = Modifier.fillMaxWidth(),
                    verticalAlignment = Alignment.CenterVertically,
                    horizontalArrangement = Arrangement.SpaceBetween,
                ) {
                    SectionHeader("Synced folders")
                    if (pairs.isNotEmpty()) {
                        Text(
                            text = if (pairs.size == 1) "1 folder" else "${pairs.size} folders",
                            style = MaterialTheme.typography.labelMedium,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                    }
                }
            }

            if (pairs.isEmpty()) {
                item {
                    EmptyState(
                        icon = Icons.Filled.CloudQueue,
                        title = "No folders yet",
                        subtitle = "Pick a folder on your server and Nimbo will keep " +
                            "it mirrored on this device.",
                        action = {
                            Button(
                                onClick = onAddFolder,
                                shape = RoundedCornerShape(14.dp),
                                modifier = Modifier.height(48.dp),
                            ) {
                                Icon(
                                    Icons.Filled.Add,
                                    contentDescription = null,
                                    modifier = Modifier.size(18.dp),
                                )
                                Spacer(Modifier.width(8.dp))
                                Text("Add folder")
                            }
                        },
                    )
                }
            } else {
                items(items = pairs, key = { it.remoteRoot }) { pair ->
                    PairRow(
                        pair = pair,
                        onRemove = {
                            deleteLocalCopy = false
                            pendingRemoval = pair.remoteRoot
                        },
                    )
                }
            }

            if (lastSynced != null) {
                item {
                    Spacer(Modifier.height(4.dp))
                    SectionHeader("Last sync")
                }
                item { LastSyncCard(lastSynced, lastSyncAt) }
            }
        }
    }

    val target = pendingRemoval
    if (target != null) {
        AlertDialog(
            onDismissRequest = { pendingRemoval = null },
            title = { Text("Stop syncing this folder?") },
            text = {
                Column {
                    Text(
                        text = displayRemote(target),
                        style = MaterialTheme.typography.bodyMedium,
                        fontWeight = FontWeight.SemiBold,
                    )
                    Spacer(Modifier.height(10.dp))
                    if (onRemoveFolderWithLocal == null) {
                        Text(
                            text = "Nimbo stops watching this folder. Files already " +
                                "on this device and on the server are kept.",
                            style = MaterialTheme.typography.bodySmall,
                        )
                    } else {
                        Text(
                            text = "Nimbo stops watching this folder. Nothing on the " +
                                "server is touched.",
                            style = MaterialTheme.typography.bodySmall,
                        )
                        Spacer(Modifier.height(8.dp))
                        Row(
                            verticalAlignment = Alignment.CenterVertically,
                            modifier = Modifier
                                .fillMaxWidth()
                                .clip(RoundedCornerShape(10.dp))
                                .clickable { deleteLocalCopy = !deleteLocalCopy }
                                .padding(end = 8.dp),
                        ) {
                            Checkbox(
                                checked = deleteLocalCopy,
                                onCheckedChange = { deleteLocalCopy = it },
                            )
                            Text(
                                text = "Also delete the local copy",
                                style = MaterialTheme.typography.bodySmall,
                            )
                        }
                    }
                }
            },
            confirmButton = {
                TextButton(onClick = {
                    pendingRemoval = null
                    val withLocal = onRemoveFolderWithLocal
                    if (withLocal != null) withLocal(target, deleteLocalCopy)
                    else onRemoveFolder(target)
                }) {
                    Text("Remove", color = MaterialTheme.colorScheme.error)
                }
            },
            dismissButton = {
                TextButton(onClick = { pendingRemoval = null }) { Text("Cancel") }
            },
        )
    }
}

private fun displayRemote(remoteRoot: String): String {
    val trimmed = remoteRoot.trim().trim('/')
    return if (trimmed.isEmpty()) "All files" else trimmed
}

@Composable
private fun PairRow(pair: SyncPair, onRemove: () -> Unit) {
    var menuOpen by remember { mutableStateOf(false) }

    Card(
        modifier = Modifier.fillMaxWidth(),
        shape = RoundedCornerShape(18.dp),
        colors = CardDefaults.cardColors(
            containerColor = MaterialTheme.colorScheme.surfaceVariant.copy(alpha = 0.45f),
        ),
    ) {
        Row(
            modifier = Modifier.padding(start = 16.dp, top = 14.dp, bottom = 14.dp, end = 4.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Box(
                modifier = Modifier
                    .size(42.dp)
                    .clip(CircleShape)
                    .background(MaterialTheme.colorScheme.primaryContainer),
                contentAlignment = Alignment.Center,
            ) {
                Icon(
                    imageVector = Icons.Filled.Folder,
                    contentDescription = null,
                    tint = MaterialTheme.colorScheme.onPrimaryContainer,
                    modifier = Modifier.size(22.dp),
                )
            }
            Spacer(Modifier.width(14.dp))
            Column(modifier = Modifier.weight(1f)) {
                Text(
                    text = displayRemote(pair.remoteRoot),
                    style = MaterialTheme.typography.titleSmall,
                    fontWeight = FontWeight.SemiBold,
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                )
                Spacer(Modifier.height(2.dp))
                Text(
                    text = pair.localDir.ifBlank { "Not linked yet" },
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                )
            }
            Box {
                IconButton(onClick = { menuOpen = true }) {
                    Icon(Icons.Filled.MoreVert, contentDescription = "Folder options")
                }
                DropdownMenu(
                    expanded = menuOpen,
                    onDismissRequest = { menuOpen = false },
                ) {
                    DropdownMenuItem(
                        text = { Text("Remove") },
                        onClick = { menuOpen = false; onRemove() },
                    )
                }
            }
        }
    }
}

@Composable
private fun QuotaCard(quota: Quota) {
    val hasLimit = quota.total > 0L
    val fraction = when {
        hasLimit -> (quota.used.toFloat() / quota.total.toFloat()).coerceIn(0f, 1f)
        quota.relative > 0.0 -> (quota.relative / 100.0).toFloat().coerceIn(0f, 1f)
        else -> 0f
    }

    Card(
        modifier = Modifier.fillMaxWidth(),
        shape = RoundedCornerShape(18.dp),
        colors = CardDefaults.cardColors(
            containerColor = MaterialTheme.colorScheme.surfaceVariant.copy(alpha = 0.45f),
        ),
    ) {
        Column(modifier = Modifier.padding(18.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Icon(
                    imageVector = Icons.Filled.Storage,
                    contentDescription = null,
                    tint = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.size(18.dp),
                )
                Spacer(Modifier.width(8.dp))
                Text(
                    text = "Server storage",
                    style = MaterialTheme.typography.titleSmall,
                    fontWeight = FontWeight.SemiBold,
                )
            }
            Spacer(Modifier.height(14.dp))
            LinearProgressIndicator(
                progress = { fraction },
                modifier = Modifier
                    .fillMaxWidth()
                    .height(8.dp)
                    .clip(RoundedCornerShape(4.dp)),
            )
            Spacer(Modifier.height(10.dp))
            Row(
                modifier = Modifier.fillMaxWidth(),
                horizontalArrangement = Arrangement.SpaceBetween,
            ) {
                Text(
                    text = if (hasLimit)
                        "${formatBytes(quota.used)} of ${formatBytes(quota.total)} used"
                    else "${formatBytes(quota.used)} used",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
                Text(
                    // "Unlimited" is quota < 0 (the server's -3 sentinel), NOT
                    // free == 0 — that is a full account, and labelling it
                    // "Unlimited" told the user the opposite of the truth at the
                    // one moment it matters.
                    text = when {
                        quota.quota < 0L -> "Unlimited"
                        quota.free > 0L -> "${formatBytes(quota.free)} free"
                        else -> "Full"
                    },
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
        }
    }
}

/**
 * "Transferred 21:41", plus "· checked 21:47" once a later pass has run and found
 * nothing to do. Uses the device's 12/24-hour setting.
 */
@Composable
private fun transferLine(transferredAt: Long, checkedAt: Long): String? {
    if (transferredAt <= 0L) return null
    val context = LocalContext.current
    val clock = remember(context) { android.text.format.DateFormat.getTimeFormat(context) }
    val transferred = "Transferred ${clock.format(java.util.Date(transferredAt))}"
    // Only worth saying separately once it is clearly a later pass.
    return if (checkedAt > transferredAt + 60_000L) {
        "$transferred · checked ${clock.format(java.util.Date(checkedAt))}"
    } else {
        transferred
    }
}

/** Why one folder stopped syncing, in the engine's own words. */
@Composable
private fun FailingFolderCard(folder: FailingFolder) {
    Card(
        modifier = Modifier.fillMaxWidth(),
        shape = RoundedCornerShape(18.dp),
        colors = CardDefaults.cardColors(
            containerColor = MaterialTheme.colorScheme.errorContainer.copy(alpha = 0.55f),
        ),
    ) {
        Column(modifier = Modifier.padding(16.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Icon(
                    imageVector = Icons.Filled.WarningAmber,
                    contentDescription = null,
                    tint = MaterialTheme.colorScheme.onErrorContainer,
                    modifier = Modifier.size(20.dp),
                )
                Spacer(Modifier.width(10.dp))
                Text(
                    text = folder.name,
                    style = MaterialTheme.typography.titleSmall,
                    fontWeight = FontWeight.SemiBold,
                    color = MaterialTheme.colorScheme.onErrorContainer,
                )
            }
            if (folder.lastError.isNotBlank()) {
                Spacer(Modifier.height(6.dp))
                Text(
                    text = folder.lastError,
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onErrorContainer,
                )
            }
            Spacer(Modifier.height(6.dp))
            Text(
                text = "Nimbo keeps retrying. Nothing has been deleted.",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onErrorContainer.copy(alpha = 0.8f),
            )
        }
    }
}

/**
 * A folder the damage guard paused.
 *
 * Deliberately loud — error colours, the engine's own reason, and a sample of
 * what would have changed — because the alternative reading is "my files are
 * quietly gone". Resuming grants a single pass, so the wording says that rather
 * than implying the folder is fixed.
 */
@Composable
private fun FrozenFolderCard(
    folder: FrozenFolder,
    busy: Boolean,
    onResume: (String) -> Unit,
) {
    Card(
        modifier = Modifier.fillMaxWidth(),
        shape = RoundedCornerShape(18.dp),
        colors = CardDefaults.cardColors(
            containerColor = MaterialTheme.colorScheme.errorContainer,
        ),
    ) {
        Column(modifier = Modifier.padding(18.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Icon(
                    imageVector = Icons.Filled.WarningAmber,
                    contentDescription = null,
                    tint = MaterialTheme.colorScheme.onErrorContainer,
                    modifier = Modifier.size(22.dp),
                )
                Spacer(Modifier.width(10.dp))
                Text(
                    text = "Syncing paused: ${folder.name}",
                    style = MaterialTheme.typography.titleSmall,
                    fontWeight = FontWeight.SemiBold,
                    color = MaterialTheme.colorScheme.onErrorContainer,
                )
            }
            Spacer(Modifier.height(8.dp))
            Text(
                text = folder.reason.ifBlank {
                    "This pass would have changed most of the folder."
                },
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onErrorContainer,
            )
            if (folder.sample.isNotEmpty()) {
                Spacer(Modifier.height(8.dp))
                Text(
                    text = "For example: " + folder.sample.take(3).joinToString(", "),
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onErrorContainer.copy(alpha = 0.8f),
                    maxLines = 2,
                    overflow = TextOverflow.Ellipsis,
                )
            }
            Spacer(Modifier.height(6.dp))
            Text(
                text = "Nothing has been changed. Check the folder is where you " +
                    "expect it before resuming.",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onErrorContainer.copy(alpha = 0.8f),
            )
            Spacer(Modifier.height(12.dp))
            Button(
                onClick = { onResume(folder.localDir) },
                enabled = !busy,
                shape = RoundedCornerShape(12.dp),
                colors = ButtonDefaults.buttonColors(
                    containerColor = MaterialTheme.colorScheme.error,
                    contentColor = MaterialTheme.colorScheme.onError,
                ),
            ) {
                Text("Resume this folder")
            }
        }
    }
}

@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun LastSyncCard(event: PairSyncedEvent, lastSyncAt: Long) {
    val stats = event.stats
    Card(
        modifier = Modifier.fillMaxWidth(),
        shape = RoundedCornerShape(18.dp),
        colors = CardDefaults.cardColors(
            containerColor = MaterialTheme.colorScheme.surfaceVariant.copy(alpha = 0.45f),
        ),
    ) {
        Column(modifier = Modifier.padding(18.dp)) {
            Text(
                text = displayRemote(event.remoteRoot),
                style = MaterialTheme.typography.titleSmall,
                fontWeight = FontWeight.SemiBold,
                maxLines = 1,
                overflow = TextOverflow.Ellipsis,
            )
            Spacer(Modifier.height(2.dp))
            Text(
                text = event.localDir,
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                maxLines = 1,
                overflow = TextOverflow.Ellipsis,
            )
            // When it transferred, and — when the engine has polled since without
            // finding anything — when it last checked. Without this the card is
            // indistinguishable from a stale result.
            transferLine(event.atMillis, lastSyncAt)?.let { line ->
                Spacer(Modifier.height(6.dp))
                Text(
                    text = line,
                    style = MaterialTheme.typography.labelSmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
            Spacer(Modifier.height(14.dp))
            // A chip per non-zero counter — a deletion-only or move-only pass used
            // to render as "0 down, 0 up", which reads as "nothing happened".
            FlowRow(
                horizontalArrangement = Arrangement.spacedBy(8.dp),
                verticalArrangement = Arrangement.spacedBy(8.dp),
            ) {
                syncStatLabels(stats).forEach { label ->
                    if (label.isProblem) {
                        StatPill(
                            text = label.text,
                            container = MaterialTheme.colorScheme.errorContainer,
                            content = MaterialTheme.colorScheme.onErrorContainer,
                        )
                    } else {
                        StatPill(label.text)
                    }
                }
            }
        }
    }
}
