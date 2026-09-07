/*
 * FolderPickerScreen.kt — stateless remote folder browser. Shows directories
 * only, a breadcrumb for the current path, and a primary action that selects the
 * folder currently being viewed for syncing.
 */
package dev.otherworld.nimbo.ui.screens

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.ArrowUpward
import androidx.compose.material.icons.filled.ChevronRight
import androidx.compose.material.icons.filled.Folder
import androidx.compose.material.icons.filled.FolderOpen
import androidx.compose.material.icons.filled.Home
import androidx.compose.material3.Button
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import dev.otherworld.nimbo.core.BrowseEntry

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun FolderPickerScreen(
    path: String,
    entries: List<BrowseEntry>,
    loading: Boolean,
    onOpen: (String) -> Unit,
    onUp: () -> Unit,
    onSelect: (String) -> Unit,
    onBack: () -> Unit,
) {
    val segments = remember(path) {
        path.trim().trim('/').split('/').filter { it.isNotBlank() }
    }
    val folders = remember(entries) { entries.filter { it.isDir } }
    val currentName = segments.lastOrNull() ?: "your whole account"

    Scaffold(
        modifier = Modifier.fillMaxSize(),
        topBar = { NimboTopBar(title = "Choose a folder", onBack = onBack) },
        bottomBar = {
            Surface(tonalElevation = 3.dp, modifier = Modifier.fillMaxWidth()) {
                Column(
                    modifier = Modifier
                        .navigationBarsPadding()
                        .padding(horizontal = 20.dp, vertical = 16.dp)
                ) {
                    Text(
                        text = "Next you'll choose where $currentName lives on this device.",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        maxLines = 2,
                        overflow = TextOverflow.Ellipsis,
                    )
                    Spacer(Modifier.height(12.dp))
                    Button(
                        onClick = { onSelect(path) },
                        enabled = !loading,
                        shape = RoundedCornerShape(14.dp),
                        modifier = Modifier
                            .fillMaxWidth()
                            .height(50.dp),
                    ) {
                        Text(
                            text = "Sync this folder",
                            style = MaterialTheme.typography.titleSmall,
                            fontWeight = FontWeight.SemiBold,
                        )
                    }
                }
            }
        },
    ) { inner ->
        Column(
            modifier = Modifier
                .fillMaxSize()
                .padding(inner),
        ) {
            Breadcrumb(segments = segments, onOpen = onOpen)
            HorizontalDivider()

            Box(modifier = Modifier.fillMaxSize()) {
                if (loading) {
                    Box(
                        modifier = Modifier.fillMaxSize(),
                        contentAlignment = Alignment.Center,
                    ) {
                        CircularProgressIndicator()
                    }
                } else if (folders.isEmpty() && segments.isEmpty()) {
                    EmptyState(
                        icon = Icons.Filled.FolderOpen,
                        title = "Nothing here",
                        subtitle = "This account has no folders yet. You can still " +
                            "sync it — anything added later comes down automatically.",
                    )
                } else {
                    LazyColumn(
                        modifier = Modifier.fillMaxSize(),
                        contentPadding = PaddingValues(
                            start = 16.dp,
                            end = 16.dp,
                            top = 12.dp,
                            bottom = 24.dp,
                        ),
                        verticalArrangement = Arrangement.spacedBy(6.dp),
                    ) {
                        if (segments.isNotEmpty()) {
                            item {
                                BrowseRow(
                                    icon = Icons.Filled.ArrowUpward,
                                    label = "..",
                                    caption = "Up one level",
                                    onClick = onUp,
                                )
                            }
                        }
                        if (folders.isEmpty()) {
                            item {
                                EmptyState(
                                    icon = Icons.Filled.FolderOpen,
                                    title = "No sub-folders",
                                    subtitle = "Tap “Sync this folder” to sync " +
                                        "everything inside it.",
                                )
                            }
                        } else {
                            items(items = folders, key = { it.path }) { entry ->
                                BrowseRow(
                                    icon = Icons.Filled.Folder,
                                    label = entry.name.ifBlank { entry.path },
                                    caption = null,
                                    onClick = { onOpen(entry.path) },
                                )
                            }
                        }
                    }
                }
            }
        }
    }
}

@Composable
private fun Breadcrumb(segments: List<String>, onOpen: (String) -> Unit) {
    val scroll = rememberScrollState()
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .horizontalScroll(scroll)
            .padding(horizontal = 16.dp, vertical = 12.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Icon(
            imageVector = Icons.Filled.Home,
            contentDescription = null,
            tint = MaterialTheme.colorScheme.onSurfaceVariant,
            modifier = Modifier
                .clip(RoundedCornerShape(8.dp))
                .clickable(enabled = segments.isNotEmpty()) { onOpen("") }
                .padding(4.dp)
                .size(18.dp),
        )
        segments.forEachIndexed { index, segment ->
            val target = segments.subList(0, index + 1).joinToString("/")
            val isLast = index == segments.lastIndex
            Icon(
                imageVector = Icons.Filled.ChevronRight,
                contentDescription = null,
                tint = MaterialTheme.colorScheme.onSurfaceVariant,
                modifier = Modifier
                    .padding(horizontal = 2.dp)
                    .size(16.dp),
            )
            Text(
                text = segment,
                style = MaterialTheme.typography.labelLarge,
                fontWeight = if (isLast) FontWeight.SemiBold else FontWeight.Normal,
                color = if (isLast) MaterialTheme.colorScheme.onSurface
                else MaterialTheme.colorScheme.onSurfaceVariant,
                maxLines = 1,
                modifier = Modifier
                    .clip(RoundedCornerShape(8.dp))
                    .clickable(enabled = !isLast) { onOpen(target) }
                    .padding(horizontal = 6.dp, vertical = 4.dp),
            )
        }
    }
}

@Composable
private fun BrowseRow(
    icon: ImageVector,
    label: String,
    caption: String?,
    onClick: () -> Unit,
) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .clip(RoundedCornerShape(16.dp))
            .clickable(onClick = onClick)
            .padding(horizontal = 10.dp, vertical = 12.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Box(
            modifier = Modifier
                .size(40.dp)
                .clip(CircleShape)
                .background(MaterialTheme.colorScheme.surfaceVariant),
            contentAlignment = Alignment.Center,
        ) {
            Icon(
                imageVector = icon,
                contentDescription = null,
                tint = MaterialTheme.colorScheme.onSurfaceVariant,
                modifier = Modifier.size(20.dp),
            )
        }
        Spacer(Modifier.width(14.dp))
        Column(modifier = Modifier.weight(1f)) {
            Text(
                text = label,
                style = MaterialTheme.typography.bodyLarge,
                maxLines = 1,
                overflow = TextOverflow.Ellipsis,
            )
            if (caption != null) {
                Text(
                    text = caption,
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
        }
        Icon(
            imageVector = Icons.Filled.ChevronRight,
            contentDescription = null,
            tint = MaterialTheme.colorScheme.onSurfaceVariant,
            modifier = Modifier.size(20.dp),
        )
    }
}
