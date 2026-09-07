/*
 * FilesScreen.kt — the browser over the whole Nextcloud account.
 *
 * What sets it apart from the official client and the web UI is the right-hand
 * column: every row says whether the file is actually on this phone. That is the
 * only thing here the server cannot tell you, so it gets real estate rather than
 * a tooltip.
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
import androidx.compose.material.icons.filled.CloudQueue
import androidx.compose.material.icons.filled.Description
import androidx.compose.material.icons.filled.Folder
import androidx.compose.material.icons.filled.FolderOpen
import androidx.compose.material.icons.filled.Home
import androidx.compose.material.icons.filled.Image
import androidx.compose.material.icons.filled.MusicNote
import androidx.compose.material.icons.filled.PictureAsPdf
import androidx.compose.material.icons.filled.Refresh
import androidx.compose.material.icons.filled.Videocam
import androidx.compose.material.icons.filled.WarningAmber
import androidx.compose.material.icons.outlined.CheckCircle
import androidx.compose.material.icons.outlined.CloudDownload
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
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
import androidx.compose.ui.graphics.vector.rememberVectorPainter
import androidx.compose.ui.layout.ContentScale
import coil.compose.AsyncImage
import dev.otherworld.nimbo.core.PreviewRequest
import androidx.compose.material.icons.filled.Add
import androidx.compose.material.icons.filled.CreateNewFolder
import androidx.compose.material.icons.filled.DeleteOutline
import androidx.compose.material.icons.filled.DriveFileRenameOutline
import androidx.compose.material.icons.filled.MoreVert
import androidx.compose.material.icons.filled.UploadFile
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.DropdownMenu
import androidx.compose.material.icons.filled.Star
import androidx.compose.material.icons.outlined.StarBorder
import androidx.compose.material.icons.filled.Share
import androidx.compose.material.icons.filled.History
import androidx.compose.material.icons.filled.Search
import androidx.compose.material.icons.filled.Notifications
import androidx.compose.material3.Badge
import androidx.compose.material3.BadgedBox
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.ExtendedFloatingActionButton
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.TextButton
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import dev.otherworld.nimbo.core.remoteNameProblem
import dev.otherworld.nimbo.core.FileRow
import dev.otherworld.nimbo.core.SyncState

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun FilesScreen(
    path: String,
    rows: List<FileRow>,
    loading: Boolean,
    error: String?,
    busy: Boolean,
    onOpenFolder: (String) -> Unit,
    onOpenFile: (FileRow) -> Unit,
    onUp: () -> Unit,
    onRefresh: () -> Unit,
    onCreateFolder: (String) -> Unit,
    onRename: (FileRow, String) -> Unit,
    onDelete: (FileRow) -> Unit,
    onUpload: () -> Unit,
    onToggleFavorite: (FileRow) -> Unit,
    onOpenVersions: (FileRow) -> Unit,
    onShare: (FileRow) -> Unit,
    onOpenFavorites: () -> Unit,
    onOpenShared: () -> Unit,
    onOpenSearch: () -> Unit,
    onOpenNotifications: () -> Unit,
    notificationCount: Int,
    onOpenTrash: () -> Unit,
) {
    val segments = remember(path) {
        path.trim().trim('/').split('/').filter { it.isNotBlank() }
    }
    var addMenuOpen by remember { mutableStateOf(false) }
    var newFolderOpen by remember { mutableStateOf(false) }
    var viewsMenuOpen by remember { mutableStateOf(false) }
    var renaming by remember { mutableStateOf<FileRow?>(null) }
    var deleting by remember { mutableStateOf<FileRow?>(null) }

    Scaffold(
        modifier = Modifier.fillMaxSize(),
        topBar = {
            Column {
                NimboTopBar(title = "Files") {
                    NotificationBell(notificationCount, onOpenNotifications)
                    IconButton(onClick = onOpenSearch) {
                        Icon(Icons.Filled.Search, contentDescription = "Search")
                    }
                    IconButton(onClick = onRefresh) {
                        Icon(Icons.Filled.Refresh, contentDescription = "Refresh")
                    }
                    // Favourites and Trash are views of the same files, so they
                    // belong here rather than beside the sync settings.
                    Box {
                        IconButton(onClick = { viewsMenuOpen = true }) {
                            Icon(Icons.Filled.MoreVert, contentDescription = "More")
                        }
                        DropdownMenu(
                            expanded = viewsMenuOpen,
                            onDismissRequest = { viewsMenuOpen = false },
                        ) {
                            DropdownMenuItem(
                                text = { Text("Favourites") },
                                leadingIcon = {
                                    Icon(Icons.Outlined.StarBorder, contentDescription = null)
                                },
                                onClick = { viewsMenuOpen = false; onOpenFavorites() },
                            )
                            DropdownMenuItem(
                                text = { Text("Shared") },
                                leadingIcon = {
                                    Icon(Icons.Filled.Share, contentDescription = null)
                                },
                                onClick = { viewsMenuOpen = false; onOpenShared() },
                            )
                            DropdownMenuItem(
                                text = { Text("Trash") },
                                leadingIcon = {
                                    Icon(Icons.Filled.DeleteOutline, contentDescription = null)
                                },
                                onClick = { viewsMenuOpen = false; onOpenTrash() },
                            )
                        }
                    }
                }
                FilesBreadcrumb(segments = segments, onOpen = onOpenFolder)
                HorizontalDivider()
                if (busy) LinearProgressIndicator(modifier = Modifier.fillMaxWidth())
            }
        },
        floatingActionButton = {
            Box {
                ExtendedFloatingActionButton(
                    onClick = { addMenuOpen = true },
                    icon = { Icon(Icons.Filled.Add, contentDescription = null) },
                    text = { Text("Add") },
                )
                DropdownMenu(expanded = addMenuOpen, onDismissRequest = { addMenuOpen = false }) {
                    DropdownMenuItem(
                        text = { Text("Upload a file") },
                        leadingIcon = { Icon(Icons.Filled.UploadFile, contentDescription = null) },
                        onClick = { addMenuOpen = false; onUpload() },
                    )
                    DropdownMenuItem(
                        text = { Text("New folder") },
                        leadingIcon = { Icon(Icons.Filled.CreateNewFolder, contentDescription = null) },
                        onClick = { addMenuOpen = false; newFolderOpen = true },
                    )
                }
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
                    title = "Couldn't open this folder",
                    subtitle = error,
                )

                rows.isEmpty() && segments.isEmpty() -> EmptyState(
                    icon = Icons.Filled.CloudQueue,
                    title = "Nothing here yet",
                    subtitle = "Files you add to Nextcloud appear here.",
                )

                rows.isEmpty() -> EmptyState(
                    icon = Icons.Filled.FolderOpen,
                    title = "Empty folder",
                    subtitle = "There is nothing in this folder.",
                )

                else -> LazyColumn(
                    modifier = Modifier.fillMaxSize(),
                    // Enough to clear the extended FAB: at 24.dp it sat on top of
                    // the last row and hid its actions.
                    contentPadding = PaddingValues(bottom = 96.dp),
                ) {
                    if (segments.isNotEmpty()) {
                        item {
                            UpRow(onUp = onUp)
                            HorizontalDivider()
                        }
                    }
                    items(items = rows, key = { it.remotePath }) { row ->
                        FileRowItem(
                            row = row,
                            onClick = {
                                if (row.isDir) onOpenFolder(row.remotePath) else onOpenFile(row)
                            },
                            onRename = { renaming = row },
                            onDelete = { deleting = row },
                        onToggleFavorite = { onToggleFavorite(row) },
                        onOpenVersions = { onOpenVersions(row) },
                        onShare = { onShare(row) },
                        )
                        HorizontalDivider()
                    }
                }
            }
        }
    }

    if (newFolderOpen) {
        NameDialog(
            title = "New folder",
            subtitle = "Created in ${folderLabel(segments)}.",
            initial = "",
            confirmLabel = "Create",
            onDismiss = { newFolderOpen = false },
            onConfirm = { name -> newFolderOpen = false; onCreateFolder(name) },
        )
    }

    renaming?.let { row ->
        NameDialog(
            title = "Rename",
            subtitle = "Renaming “${row.name}”.",
            initial = row.name,
            confirmLabel = "Rename",
            onDismiss = { renaming = null },
            onConfirm = { name -> renaming = null; onRename(row, name) },
        )
    }

    deleting?.let { row ->
        DeleteDialog(
            row = row,
            onDismiss = { deleting = null },
            onConfirm = { deleting = null; onDelete(row) },
        )
    }
}

private fun folderLabel(segments: List<String>): String =
    segments.lastOrNull() ?: "your account's top level"

/** Shared name prompt for "new folder" and "rename", with live validation. */
@Composable
private fun NameDialog(
    title: String,
    subtitle: String,
    initial: String,
    confirmLabel: String,
    onDismiss: () -> Unit,
    onConfirm: (String) -> Unit,
) {
    var value by remember { mutableStateOf(initial) }
    val problem = remember(value) { if (value.isEmpty()) null else remoteNameProblem(value) }
    val canConfirm = value.isNotBlank() && problem == null

    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text(title) },
        text = {
            Column {
                Text(subtitle, style = MaterialTheme.typography.bodyMedium)
                Spacer(Modifier.height(12.dp))
                OutlinedTextField(
                    value = value,
                    onValueChange = { value = it },
                    singleLine = true,
                    isError = problem != null,
                    label = { Text("Name") },
                    supportingText = problem?.let { { Text(it) } },
                )
            }
        },
        confirmButton = {
            TextButton(onClick = { onConfirm(value.trim()) }, enabled = canConfirm) {
                Text(confirmLabel)
            }
        },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
    )
}

/**
 * Deletion confirm. It names the file and says plainly what else goes: a file
 * inside a synced folder loses its local copy on the next pass, which is not
 * obvious from a screen that is showing you the server.
 */
@Composable
private fun DeleteDialog(row: FileRow, onDismiss: () -> Unit, onConfirm: () -> Unit) {
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text(if (row.isDir) "Delete folder?" else "Delete file?") },
        text = {
            Column {
                Text("“${row.name}” will be deleted from your Nextcloud.")
                if (row.syncState != SyncState.SERVER_ONLY) {
                    Spacer(Modifier.height(8.dp))
                    Text(
                        "It's in a synced folder, so the copy on this phone goes too " +
                            "the next time Nimbo syncs.",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
                Spacer(Modifier.height(8.dp))
                Text(
                    "If your server keeps a trashbin you can restore it from Trash, under the menu on the Sync tab.",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
        },
        confirmButton = {
            TextButton(onClick = onConfirm) {
                Text("Delete", color = MaterialTheme.colorScheme.error)
            }
        },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
    )
}

@Composable
private fun FilesBreadcrumb(segments: List<String>, onOpen: (String) -> Unit) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .horizontalScroll(rememberScrollState())
            .padding(horizontal = 16.dp, vertical = 10.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Icon(
            imageVector = Icons.Filled.Home,
            contentDescription = "Account root",
            modifier = Modifier
                .size(20.dp)
                .clip(CircleShape)
                .clickable { onOpen("") },
            tint = if (segments.isEmpty()) MaterialTheme.colorScheme.onSurface
            else MaterialTheme.colorScheme.onSurfaceVariant,
        )
        segments.forEachIndexed { index, segment ->
            Icon(
                imageVector = Icons.Filled.ChevronRight,
                contentDescription = null,
                modifier = Modifier.size(18.dp),
                tint = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            val target = segments.take(index + 1).joinToString("/")
            Text(
                text = segment,
                style = MaterialTheme.typography.bodyMedium,
                fontWeight = if (index == segments.lastIndex) FontWeight.SemiBold else FontWeight.Normal,
                color = if (index == segments.lastIndex) MaterialTheme.colorScheme.onSurface
                else MaterialTheme.colorScheme.onSurfaceVariant,
                maxLines = 1,
                modifier = Modifier
                    .clip(RoundedCornerShape(6.dp))
                    .clickable { onOpen(target) }
                    .padding(horizontal = 4.dp, vertical = 2.dp),
            )
        }
    }
}

@Composable
private fun UpRow(onUp: () -> Unit) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .clickable(onClick = onUp)
            .padding(horizontal = 16.dp, vertical = 14.dp),
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
                imageVector = Icons.Filled.ArrowUpward,
                contentDescription = null,
                modifier = Modifier.size(20.dp),
                tint = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
        Spacer(Modifier.width(14.dp))
        Text("..", style = MaterialTheme.typography.bodyLarge)
    }
}

@Composable
private fun FileRowItem(
    row: FileRow,
    onClick: () -> Unit,
    onRename: () -> Unit,
    onDelete: () -> Unit,
    onToggleFavorite: () -> Unit,
    onOpenVersions: () -> Unit,
    onShare: () -> Unit,
) {
    var menuOpen by remember { mutableStateOf(false) }
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .clickable(onClick = onClick)
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
            // A server-rendered preview when there is one, the type icon while it
            // loads or when there is not. AsyncImage falls back on its own when
            // the fetcher returns null, so an un-previewable file is not an error.
            if (row.isPreviewable) {
                AsyncImage(
                    model = PreviewRequest(row.fileId, px = 128),
                    contentDescription = null,
                    contentScale = ContentScale.Crop,
                    modifier = Modifier.fillMaxSize(),
                    placeholder = rememberVectorPainter(iconFor(row)),
                    error = rememberVectorPainter(iconFor(row)),
                    fallback = rememberVectorPainter(iconFor(row)),
                )
            } else {
                Icon(
                    imageVector = iconFor(row),
                    contentDescription = null,
                    modifier = Modifier.size(22.dp),
                    tint = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
        }
        Spacer(Modifier.width(14.dp))
        Column(modifier = Modifier.weight(1f)) {
            Text(
                text = row.name,
                style = MaterialTheme.typography.bodyLarge,
                maxLines = 1,
                overflow = TextOverflow.Ellipsis,
            )
            val subtitle = if (row.isDir) "Folder" else formatBytes(row.size)
            Text(
                text = subtitle,
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                maxLines = 1,
            )
        }
        Spacer(Modifier.width(10.dp))
        if (row.isFavorite) {
            Icon(
                Icons.Filled.Star,
                contentDescription = "Favourite",
                tint = MaterialTheme.colorScheme.primary,
                modifier = Modifier.size(18.dp),
            )
            Spacer(Modifier.width(6.dp))
        }
        SyncBadge(row.syncState)
        Box {
            IconButton(onClick = { menuOpen = true }) {
                Icon(
                    Icons.Filled.MoreVert,
                    contentDescription = "Actions for ${row.name}",
                    tint = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
            DropdownMenu(expanded = menuOpen, onDismissRequest = { menuOpen = false }) {
                DropdownMenuItem(
                    text = { Text("Share") },
                    leadingIcon = { Icon(Icons.Filled.Share, contentDescription = null) },
                    onClick = { menuOpen = false; onShare() },
                )
                DropdownMenuItem(
                    text = {
                        Text(if (row.isFavorite) "Remove from favourites" else "Add to favourites")
                    },
                    leadingIcon = {
                        Icon(
                            if (row.isFavorite) Icons.Filled.Star else Icons.Outlined.StarBorder,
                            contentDescription = null,
                        )
                    },
                    onClick = { menuOpen = false; onToggleFavorite() },
                )
                if (!row.isDir) {
                    DropdownMenuItem(
                        text = { Text("Version history") },
                        leadingIcon = { Icon(Icons.Filled.History, contentDescription = null) },
                        onClick = { menuOpen = false; onOpenVersions() },
                    )
                }
                DropdownMenuItem(
                    text = { Text("Rename") },
                    leadingIcon = { Icon(Icons.Filled.DriveFileRenameOutline, contentDescription = null) },
                    onClick = { menuOpen = false; onRename() },
                )
                DropdownMenuItem(
                    text = { Text("Delete") },
                    leadingIcon = {
                        Icon(
                            Icons.Filled.DeleteOutline,
                            contentDescription = null,
                            tint = MaterialTheme.colorScheme.error,
                        )
                    },
                    onClick = { menuOpen = false; onDelete() },
                )
            }
        }
    }
}

/**
 * The differentiator, in one glyph: on this phone, still coming, server-only, or
 * needs attention. Carries a text label as its content description so it is not
 * colour-and-shape only.
 */
@Composable
internal fun SyncBadge(state: SyncState) {
    val (icon, tint, label) = when (state) {
        SyncState.SYNCED -> Triple(
            Icons.Outlined.CheckCircle,
            MaterialTheme.colorScheme.primary,
            "On this phone",
        )
        SyncState.PENDING -> Triple(
            Icons.Outlined.CloudDownload,
            MaterialTheme.colorScheme.onSurfaceVariant,
            "Waiting to sync",
        )
        SyncState.SERVER_ONLY -> Triple(
            Icons.Filled.CloudQueue,
            MaterialTheme.colorScheme.onSurfaceVariant.copy(alpha = 0.55f),
            "On the server only",
        )
        SyncState.CONFLICTED -> Triple(
            Icons.Filled.WarningAmber,
            MaterialTheme.colorScheme.error,
            "Needs attention",
        )
    }
    Icon(
        imageVector = icon,
        contentDescription = label,
        tint = tint,
        modifier = Modifier.size(20.dp),
    )
}

/** A type icon from the content type, falling back to the extension. */
private fun iconFor(row: FileRow): ImageVector {
    if (row.isDir) return Icons.Filled.Folder
    val type = row.contentType.lowercase()
    val ext = row.name.substringAfterLast('.', "").lowercase()
    return when {
        type.startsWith("image/") || ext in IMAGE_EXT -> Icons.Filled.Image
        type.startsWith("video/") || ext in VIDEO_EXT -> Icons.Filled.Videocam
        type.startsWith("audio/") || ext in AUDIO_EXT -> Icons.Filled.MusicNote
        type == "application/pdf" || ext == "pdf" -> Icons.Filled.PictureAsPdf
        else -> Icons.Filled.Description
    }
}

private val IMAGE_EXT = setOf("jpg", "jpeg", "png", "gif", "webp", "heic", "bmp", "svg")
private val VIDEO_EXT = setOf("mp4", "mkv", "mov", "avi", "webm", "m4v")
private val AUDIO_EXT = setOf("mp3", "flac", "ogg", "wav", "m4a", "opus")
