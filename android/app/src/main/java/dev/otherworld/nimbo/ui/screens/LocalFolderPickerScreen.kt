/*
 * LocalFolderPickerScreen.kt — stateless browser for shared storage. Mirrors
 * FolderPickerScreen (the remote side) so picking "where on the server" and
 * "where on the phone" feel like the same gesture: folders only, a location
 * bar, an up row, and one primary action that takes the folder being viewed.
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
import androidx.compose.material.icons.filled.CreateNewFolder
import androidx.compose.material.icons.filled.Folder
import androidx.compose.material.icons.filled.FolderOpen
import androidx.compose.material.icons.filled.Lock
import androidx.compose.material.icons.filled.PhoneAndroid
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Surface
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
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import dev.otherworld.nimbo.platform.LocalDir
import dev.otherworld.nimbo.platform.folderSubtitle
import dev.otherworld.nimbo.platform.LocalFs

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun LocalFolderPickerScreen(
    path: String,
    dirs: List<LocalDir>,
    canGoUp: Boolean,
    onOpen: (String) -> Unit,
    onUp: () -> Unit,
    onCreateFolder: (String) -> Unit,
    onUse: (String) -> Unit,
    onBack: () -> Unit,
) {
    var creating by remember { mutableStateOf(false) }
    val pretty = remember(path) { LocalFs.displayPath(path) }
    val writable = remember(path, dirs) { LocalFs.isWritable(path) }

    Scaffold(
        modifier = Modifier.fillMaxSize(),
        topBar = {
            NimboTopBar(title = "Choose location", onBack = onBack) {
                IconButton(onClick = { creating = true }) {
                    Icon(Icons.Filled.CreateNewFolder, contentDescription = "New folder")
                }
            }
        },
        bottomBar = {
            Surface(tonalElevation = 3.dp, modifier = Modifier.fillMaxWidth()) {
                Column(
                    modifier = Modifier
                        .navigationBarsPadding()
                        .padding(horizontal = 20.dp, vertical = 16.dp)
                ) {
                    Text(
                        text = "Files will be kept in",
                        style = MaterialTheme.typography.labelMedium,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                    Spacer(Modifier.height(3.dp))
                    Text(
                        text = pretty,
                        style = MaterialTheme.typography.bodyMedium,
                        fontWeight = FontWeight.SemiBold,
                        maxLines = 2,
                        overflow = TextOverflow.Ellipsis,
                    )
                    if (!writable) {
                        Spacer(Modifier.height(8.dp))
                        Row(verticalAlignment = Alignment.CenterVertically) {
                            Icon(
                                imageVector = Icons.Filled.Lock,
                                contentDescription = null,
                                tint = MaterialTheme.colorScheme.error,
                                modifier = Modifier.size(16.dp),
                            )
                            Spacer(Modifier.width(6.dp))
                            Text(
                                text = "Nimbo can’t write here — pick or create another folder.",
                                style = MaterialTheme.typography.bodySmall,
                                color = MaterialTheme.colorScheme.error,
                                maxLines = 2,
                                overflow = TextOverflow.Ellipsis,
                            )
                        }
                    }
                    Spacer(Modifier.height(12.dp))
                    Button(
                        onClick = { onUse(path) },
                        enabled = writable,
                        shape = RoundedCornerShape(14.dp),
                        modifier = Modifier
                            .fillMaxWidth()
                            .height(50.dp),
                    ) {
                        Text(
                            text = "Sync here",
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
            LocationBar(pretty = pretty)
            HorizontalDivider()

            Box(modifier = Modifier.fillMaxSize()) {
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
                    if (canGoUp) {
                        item {
                            LocalDirRow(
                                icon = Icons.Filled.ArrowUpward,
                                label = "..",
                                caption = "Up one level",
                                onClick = onUp,
                            )
                        }
                    }

                    item {
                        LocalDirRow(
                            icon = Icons.Filled.CreateNewFolder,
                            label = "New folder",
                            caption = "Create one inside this location",
                            onClick = { creating = true },
                        )
                    }

                    if (dirs.isEmpty()) {
                        item {
                            EmptyState(
                                icon = Icons.Filled.FolderOpen,
                                title = "No folders here",
                                subtitle = "Tap “Sync here” to use this location, or " +
                                    "create a new folder for it.",
                            )
                        }
                    } else {
                        items(items = dirs, key = { it.path }) { dir ->
                            LocalDirRow(
                                icon = Icons.Filled.Folder,
                                label = dir.name.ifBlank { dir.path },
                                // Counts files as well as folders: a folder of
                                // files was previously described as "Empty".
                                caption = folderSubtitle(
                                    dirs = dir.childDirCount,
                                    files = dir.childFileCount,
                                ),
                                onClick = { onOpen(dir.path) },
                            )
                        }
                    }
                }
            }
        }
    }

    if (creating) {
        NewFolderDialog(
            location = pretty,
            onDismiss = { creating = false },
            onCreate = { name ->
                creating = false
                onCreateFolder(name)
            },
        )
    }
}

@Composable
private fun LocationBar(pretty: String) {
    val scroll = rememberScrollState()
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .horizontalScroll(scroll)
            .padding(horizontal = 16.dp, vertical = 12.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Icon(
            imageVector = Icons.Filled.PhoneAndroid,
            contentDescription = null,
            tint = MaterialTheme.colorScheme.onSurfaceVariant,
            modifier = Modifier
                .padding(4.dp)
                .size(18.dp),
        )
        val segments = remember(pretty) { pretty.split('/').filter { it.isNotBlank() } }
        segments.forEachIndexed { index, segment ->
            if (index > 0) {
                Icon(
                    imageVector = Icons.Filled.ChevronRight,
                    contentDescription = null,
                    tint = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier
                        .padding(horizontal = 2.dp)
                        .size(16.dp),
                )
            }
            val isLast = index == segments.lastIndex
            Text(
                text = segment,
                style = MaterialTheme.typography.labelLarge,
                fontWeight = if (isLast) FontWeight.SemiBold else FontWeight.Normal,
                color = if (isLast) MaterialTheme.colorScheme.onSurface
                else MaterialTheme.colorScheme.onSurfaceVariant,
                maxLines = 1,
                modifier = Modifier.padding(horizontal = 6.dp, vertical = 4.dp),
            )
        }
    }
}

@Composable
private fun LocalDirRow(
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

@Composable
private fun NewFolderDialog(
    location: String,
    onDismiss: () -> Unit,
    onCreate: (String) -> Unit,
) {
    var name by remember { mutableStateOf("") }
    val trimmed = name.trim()

    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("New folder") },
        text = {
            Column {
                Text(
                    text = "Created inside $location.",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
                Spacer(Modifier.height(14.dp))
                OutlinedTextField(
                    value = name,
                    onValueChange = { name = it },
                    label = { Text("Folder name") },
                    singleLine = true,
                    shape = RoundedCornerShape(14.dp),
                    modifier = Modifier.fillMaxWidth(),
                )
            }
        },
        confirmButton = {
            TextButton(
                onClick = { onCreate(trimmed) },
                enabled = trimmed.isNotEmpty(),
            ) {
                Text("Create")
            }
        },
        dismissButton = {
            TextButton(onClick = onDismiss) { Text("Cancel") }
        },
    )
}
