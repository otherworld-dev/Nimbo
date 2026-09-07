/*
 * VersionsSheet.kt — the file's history, over the browser rather than instead
 * of it.
 *
 * A version list is a list of dates: the date is the only thing telling one
 * revision from another, so it is what each row leads with.
 */
package dev.otherworld.nimbo.ui.screens

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.History
import androidx.compose.material.icons.filled.Restore
import androidx.compose.material.icons.filled.WarningAmber
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.rememberModalBottomSheetState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import dev.otherworld.nimbo.core.FileRow
import dev.otherworld.nimbo.core.FileVersion

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun VersionsSheet(
    row: FileRow,
    versions: List<FileVersion>,
    loading: Boolean,
    error: String?,
    actionError: String?,
    busy: Boolean,
    onRestore: (FileVersion) -> Unit,
    onDismiss: () -> Unit,
) {
    val sheetState = rememberModalBottomSheetState(skipPartiallyExpanded = true)
    var confirming by remember { mutableStateOf<FileVersion?>(null) }

    ModalBottomSheet(onDismissRequest = onDismiss, sheetState = sheetState) {
        Column(modifier = Modifier.padding(bottom = 24.dp)) {
            Column(modifier = Modifier.padding(horizontal = 24.dp)) {
                Text("Version history", style = MaterialTheme.typography.titleMedium)
                Spacer(Modifier.height(2.dp))
                Text(
                    text = row.name,
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                )
            }
            Spacer(Modifier.height(12.dp))
            HorizontalDivider()
            SheetError(actionError)

            when {
                loading && versions.isEmpty() -> Box(
                    modifier = Modifier
                        .fillMaxWidth()
                        .height(140.dp),
                    contentAlignment = Alignment.Center,
                ) { CircularProgressIndicator() }

                error != null -> SheetNotice(
                    icon = Icons.Filled.WarningAmber,
                    text = error,
                    tint = MaterialTheme.colorScheme.error,
                )

                // Two different facts look the same from here, so the wording
                // covers both rather than picking one and being wrong half the time.
                versions.isEmpty() -> SheetNotice(
                    icon = Icons.Filled.History,
                    text = "No earlier versions. Either this file hasn't changed " +
                        "since it was uploaded, or your server doesn't keep versions.",
                    tint = MaterialTheme.colorScheme.onSurfaceVariant,
                )

                else -> LazyColumn(modifier = Modifier.heightIn(max = 420.dp)) {
                    items(items = versions, key = { it.href }) { version ->
                        VersionRow(
                            version = version,
                            enabled = !busy,
                            onRestore = { confirming = version },
                        )
                        HorizontalDivider()
                    }
                }
            }
        }
    }

    confirming?.let { version ->
        AlertDialog(
            onDismissRequest = { confirming = null },
            title = { Text("Restore this version?") },
            text = {
                Column {
                    Text("“${row.name}” goes back to how it was on ${version.whenLabel}.")
                    Spacer(Modifier.height(8.dp))
                    // Worth saying: this reads as destructive but isn't.
                    Text(
                        "The current contents become a version too, so you can " +
                            "come back to them.",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            },
            confirmButton = {
                TextButton(onClick = { confirming = null; onRestore(version) }) { Text("Restore") }
            },
            dismissButton = { TextButton(onClick = { confirming = null }) { Text("Cancel") } },
        )
    }
}

@Composable
private fun SheetNotice(
    icon: androidx.compose.ui.graphics.vector.ImageVector,
    text: String,
    tint: androidx.compose.ui.graphics.Color,
) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .padding(horizontal = 24.dp, vertical = 24.dp),
        verticalAlignment = Alignment.Top,
    ) {
        Icon(icon, contentDescription = null, tint = tint, modifier = Modifier.size(20.dp))
        Spacer(Modifier.width(12.dp))
        Text(
            text = text,
            style = MaterialTheme.typography.bodyMedium,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }
}

@Composable
private fun VersionRow(
    version: FileVersion,
    enabled: Boolean,
    onRestore: () -> Unit,
) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .padding(start = 24.dp, end = 12.dp, top = 14.dp, bottom = 14.dp),
        verticalAlignment = Alignment.CenterVertically,
        horizontalArrangement = Arrangement.SpaceBetween,
    ) {
        Column(modifier = Modifier.weight(1f)) {
            Text(version.whenLabel, style = MaterialTheme.typography.bodyLarge)
            if (version.size > 0) {
                Text(
                    text = formatBytes(version.size),
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
        }
        TextButton(onClick = onRestore, enabled = enabled) {
            Icon(Icons.Filled.Restore, contentDescription = null, modifier = Modifier.size(18.dp))
            Spacer(Modifier.width(6.dp))
            Text("Restore")
        }
    }
}
