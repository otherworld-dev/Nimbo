/*
 * FavoritesScreen.kt — the user's starred files and folders, gathered from
 * across the whole account.
 *
 * Not a directory listing: these rows come from everywhere, so each one names
 * the folder it lives in. Without that, two starred files called "report.pdf"
 * are indistinguishable.
 */
package dev.otherworld.nimbo.ui.screens

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Folder
import androidx.compose.material.icons.filled.InsertDriveFile
import androidx.compose.material.icons.filled.Refresh
import androidx.compose.material.icons.filled.Star
import androidx.compose.material.icons.filled.WarningAmber
import androidx.compose.material.icons.outlined.StarBorder
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import dev.otherworld.nimbo.core.FileRow

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun FavoritesScreen(
    rows: List<FileRow>,
    loading: Boolean,
    error: String?,
    busy: Boolean,
    onOpen: (FileRow) -> Unit,
    onToggleFavorite: (FileRow) -> Unit,
    onRefresh: () -> Unit,
    onBack: () -> Unit,
) {
    Scaffold(
        modifier = Modifier.fillMaxSize(),
        topBar = {
            Column {
                NimboTopBar(title = "Favourites", onBack = onBack) {
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
                loading && rows.isEmpty() -> Box(
                    modifier = Modifier.fillMaxSize(),
                    contentAlignment = Alignment.Center,
                ) { CircularProgressIndicator() }

                error != null -> EmptyState(
                    icon = Icons.Filled.WarningAmber,
                    title = "Couldn't load favourites",
                    subtitle = error,
                )

                rows.isEmpty() -> EmptyState(
                    icon = Icons.Outlined.StarBorder,
                    title = "No favourites yet",
                    subtitle = "Star a file or folder from its menu in Files and it will appear here.",
                )

                else -> LazyColumn(
                    modifier = Modifier.fillMaxSize(),
                    contentPadding = PaddingValues(bottom = 24.dp),
                ) {
                    items(items = rows, key = { it.remotePath }) { row ->
                        FavoriteRow(
                            row = row,
                            enabled = !busy,
                            onOpen = { onOpen(row) },
                            onUnstar = { onToggleFavorite(row) },
                        )
                        HorizontalDivider()
                    }
                }
            }
        }
    }
}

@Composable
private fun FavoriteRow(
    row: FileRow,
    enabled: Boolean,
    onOpen: () -> Unit,
    onUnstar: () -> Unit,
) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .clickable(onClick = onOpen)
            .padding(start = 16.dp, end = 4.dp, top = 12.dp, bottom = 12.dp),
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
                imageVector = if (row.isDir) Icons.Filled.Folder else Icons.Filled.InsertDriveFile,
                contentDescription = null,
                modifier = Modifier.size(22.dp),
                tint = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
        Spacer(Modifier.width(14.dp))
        Column(modifier = Modifier.weight(1f)) {
            Text(
                text = row.name,
                style = MaterialTheme.typography.bodyLarge,
                maxLines = 1,
                overflow = TextOverflow.Ellipsis,
            )
            Text(
                text = buildString {
                    append(parentLabel(row.remotePath))
                    if (!row.isDir && row.size > 0) {
                        append(" · ")
                        append(formatBytes(row.size))
                    }
                },
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                maxLines = 1,
                overflow = TextOverflow.Ellipsis,
            )
        }
        Spacer(Modifier.width(10.dp))
        SyncBadge(row.syncState)
        IconButton(onClick = onUnstar, enabled = enabled) {
            Icon(
                Icons.Filled.Star,
                contentDescription = "Remove ${row.name} from favourites",
                tint = MaterialTheme.colorScheme.primary,
            )
        }
    }
}

/**
 * The folder an account-relative path sits in, as a person would say it.
 * Top-level items report "Files", matching what Nextcloud calls the root.
 */
internal fun parentLabel(remotePath: String): String {
    val path = remotePath.trim().trim('/')
    if (!path.contains('/')) return "Files"
    return path.substringBeforeLast('/')
}
