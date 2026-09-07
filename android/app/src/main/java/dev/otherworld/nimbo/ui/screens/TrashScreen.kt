/*
 * TrashScreen.kt — the server trashbin: what Nextcloud kept after a delete, and
 * the two things you can do about it.
 *
 * This is the undo the file browser's delete dialog promises when it says "if
 * your server's trash is enabled you can restore it there" — this is there.
 */
package dev.otherworld.nimbo.ui.screens

import androidx.compose.foundation.background
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
import androidx.compose.material.icons.filled.DeleteForever
import androidx.compose.material.icons.filled.DeleteOutline
import androidx.compose.material.icons.filled.Folder
import androidx.compose.material.icons.filled.InsertDriveFile
import androidx.compose.material.icons.filled.Refresh
import androidx.compose.material.icons.filled.Restore
import androidx.compose.material.icons.filled.WarningAmber
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
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
import androidx.compose.ui.draw.clip
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import dev.otherworld.nimbo.core.TrashItem

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun TrashScreen(
    items: List<TrashItem>,
    loading: Boolean,
    error: String?,
    busy: Boolean,
    onRestore: (TrashItem) -> Unit,
    onDeleteForever: (TrashItem) -> Unit,
    onRefresh: () -> Unit,
    onBack: () -> Unit,
) {
    var confirming by remember { mutableStateOf<TrashItem?>(null) }

    Scaffold(
        modifier = Modifier.fillMaxSize(),
        topBar = {
            Column {
                NimboTopBar(title = "Trash", onBack = onBack) {
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
                    title = "Couldn't open the trash",
                    subtitle = error,
                )

                // Deliberately hedged: an empty list also means the server has the
                // trashbin app switched off, and the two are indistinguishable here.
                items.isEmpty() -> EmptyState(
                    icon = Icons.Filled.DeleteOutline,
                    title = "Nothing in the trash",
                    subtitle = "Deleted files appear here if your server keeps a trashbin.",
                )

                else -> LazyColumn(
                    modifier = Modifier.fillMaxSize(),
                    contentPadding = PaddingValues(bottom = 24.dp),
                ) {
                    items(items = items, key = { it.href }) { item ->
                        TrashRow(
                            item = item,
                            enabled = !busy,
                            onRestore = { onRestore(item) },
                            onDeleteForever = { confirming = item },
                        )
                        HorizontalDivider()
                    }
                }
            }
        }
    }

    confirming?.let { item ->
        AlertDialog(
            onDismissRequest = { confirming = null },
            title = { Text("Delete permanently?") },
            text = {
                Column {
                    Text("“${item.name}” will be gone for good.")
                    Spacer(Modifier.height(8.dp))
                    Text(
                        "This is the last copy your server has. There is no further undo.",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            },
            confirmButton = {
                TextButton(onClick = { confirming = null; onDeleteForever(item) }) {
                    Text("Delete forever", color = MaterialTheme.colorScheme.error)
                }
            },
            dismissButton = { TextButton(onClick = { confirming = null }) { Text("Cancel") } },
        )
    }
}

@Composable
private fun TrashRow(
    item: TrashItem,
    enabled: Boolean,
    onRestore: () -> Unit,
    onDeleteForever: () -> Unit,
) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .padding(horizontal = 16.dp, vertical = 12.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Box(
            modifier = Modifier
                .size(40.dp)
                .clip(RoundedCornerShape(10.dp))
                .background(MaterialTheme.colorScheme.surfaceVariant),
            contentAlignment = Alignment.Center,
        ) {
            Icon(
                imageVector = if (item.isDir) Icons.Filled.Folder else Icons.Filled.InsertDriveFile,
                contentDescription = null,
                modifier = Modifier.size(22.dp),
                tint = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
        Spacer(Modifier.width(14.dp))
        Column(modifier = Modifier.weight(1f)) {
            Text(
                text = item.name,
                style = MaterialTheme.typography.bodyLarge,
                maxLines = 1,
                overflow = TextOverflow.Ellipsis,
            )
            // Where it came from matters more than its size here: the user is
            // deciding whether this is the thing they meant to lose.
            Text(
                text = buildString {
                    append("from ")
                    append(item.originalFolder)
                    if (!item.isDir && item.size > 0) {
                        append(" · ")
                        append(formatBytes(item.size))
                    }
                },
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                maxLines = 1,
                overflow = TextOverflow.Ellipsis,
            )
        }
        IconButton(onClick = onRestore, enabled = enabled) {
            Icon(
                Icons.Filled.Restore,
                contentDescription = "Restore ${item.name}",
                tint = MaterialTheme.colorScheme.primary,
            )
        }
        IconButton(onClick = onDeleteForever, enabled = enabled) {
            Icon(
                Icons.Filled.DeleteForever,
                contentDescription = "Delete ${item.name} permanently",
                tint = MaterialTheme.colorScheme.error,
            )
        }
    }
}
