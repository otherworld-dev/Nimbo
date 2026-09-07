/*
 * SearchScreen.kt — find a file anywhere in the account by name.
 *
 * Names, not contents. The screen says so up front rather than letting a user
 * conclude their document isn't on the server when it was only its text that
 * didn't match.
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
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Close
import androidx.compose.material.icons.filled.Folder
import androidx.compose.material.icons.filled.InsertDriveFile
import androidx.compose.material.icons.filled.Search
import androidx.compose.material.icons.filled.SearchOff
import androidx.compose.material.icons.filled.WarningAmber
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.platform.LocalSoftwareKeyboardController
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import dev.otherworld.nimbo.core.FileRow

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun SearchScreen(
    term: String,
    results: List<FileRow>,
    loading: Boolean,
    error: String?,
    hasSearched: Boolean,
    truncated: Boolean,
    onTermChange: (String) -> Unit,
    onSubmit: () -> Unit,
    onOpen: (FileRow) -> Unit,
    onBack: () -> Unit,
) {
    val focusRequester = remember { FocusRequester() }
    val keyboard = LocalSoftwareKeyboardController.current

    // The user came here to type; open with the caret already in the field.
    LaunchedEffect(Unit) {
        runCatching { focusRequester.requestFocus() }
    }

    Scaffold(
        modifier = Modifier.fillMaxSize(),
        topBar = {
            Column {
                NimboTopBar(title = "Search", onBack = onBack)
                OutlinedTextField(
                    value = term,
                    onValueChange = onTermChange,
                    modifier = Modifier
                        .fillMaxWidth()
                        .padding(horizontal = 16.dp, vertical = 8.dp)
                        .focusRequester(focusRequester),
                    singleLine = true,
                    label = { Text("File or folder name") },
                    leadingIcon = { Icon(Icons.Filled.Search, contentDescription = null) },
                    trailingIcon = {
                        if (term.isNotEmpty()) {
                            IconButton(onClick = { onTermChange("") }) {
                                Icon(Icons.Filled.Close, contentDescription = "Clear")
                            }
                        }
                    },
                    keyboardOptions = KeyboardOptions(imeAction = ImeAction.Search),
                    keyboardActions = KeyboardActions(onSearch = {
                        keyboard?.hide()
                        onSubmit()
                    }),
                )
                if (loading) LinearProgressIndicator(modifier = Modifier.fillMaxWidth())
                HorizontalDivider()
            }
        },
    ) { inner ->
        Box(
            modifier = Modifier
                .fillMaxSize()
                .padding(inner),
        ) {
            when {
                loading && results.isEmpty() -> Box(
                    modifier = Modifier.fillMaxSize(),
                    contentAlignment = Alignment.Center,
                ) { CircularProgressIndicator() }

                error != null -> EmptyState(
                    icon = Icons.Filled.WarningAmber,
                    title = "Search failed",
                    subtitle = error,
                )

                // Before the first search, say what this does and does not do,
                // so an empty result later is not read as "it isn't on the server".
                !hasSearched -> EmptyState(
                    icon = Icons.Filled.Search,
                    title = "Search your files",
                    subtitle = "Matches names anywhere in your account — not what's inside files.",
                )

                results.isEmpty() -> EmptyState(
                    icon = Icons.Filled.SearchOff,
                    title = "No matches for “$term”",
                    subtitle = "Only names are searched, so a file whose contents mention it won't appear.",
                )

                else -> LazyColumn(
                    modifier = Modifier.fillMaxSize(),
                    contentPadding = PaddingValues(bottom = 24.dp),
                ) {
                    item {
                        Text(
                            text = when {
                                // A full page is not a count. Saying "50 matches"
                                // when the server had 400 would be a wrong answer
                                // delivered confidently.
                                truncated -> "First ${results.size} matches — narrow the search to see more"
                                results.size == 1 -> "1 match"
                                else -> "${results.size} matches"
                            },
                            style = MaterialTheme.typography.labelMedium,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                            modifier = Modifier.padding(
                                start = 16.dp, end = 16.dp, top = 12.dp, bottom = 4.dp,
                            ),
                        )
                    }
                    items(items = results, key = { it.remotePath }) { row ->
                        SearchHitRow(row = row, onClick = { onOpen(row) })
                        HorizontalDivider()
                    }
                }
            }
        }
    }
}

@Composable
private fun SearchHitRow(row: FileRow, onClick: () -> Unit) {
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
            // Hits come from all over the account, so where it lives is the
            // thing that tells two same-named files apart.
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
        Spacer(Modifier.width(8.dp))
    }
}
