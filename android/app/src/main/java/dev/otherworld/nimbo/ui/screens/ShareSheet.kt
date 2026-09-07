/*
 * ShareSheet.kt — who can reach one file, and giving or taking that away.
 *
 * The existing shares come first. Someone opening this is usually asking "who
 * can already see this?" as often as "let me send it to Bob", and answering the
 * first question before offering the second is the safer order.
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
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.ContentCopy
import androidx.compose.material.icons.filled.Group
import androidx.compose.material.icons.filled.Link
import androidx.compose.material.icons.filled.LinkOff
import androidx.compose.material.icons.filled.Lock
import androidx.compose.material.icons.filled.Mail
import androidx.compose.material.icons.filled.Person
import androidx.compose.material.icons.filled.PersonAdd
import androidx.compose.material.icons.filled.Public
import androidx.compose.material.icons.filled.Share
import androidx.compose.material.icons.filled.WarningAmber
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
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
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import dev.otherworld.nimbo.core.FileRow
import dev.otherworld.nimbo.core.Share

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun ShareSheet(
    row: FileRow,
    shares: List<Share>,
    loading: Boolean,
    error: String?,
    actionError: String?,
    busy: Boolean,
    onCreateLink: (password: String, expiration: String) -> Unit,
    onShareWithUser: (String) -> Unit,
    onRevoke: (Share) -> Unit,
    onCopyLink: (Share) -> Unit,
    onDismiss: () -> Unit,
) {
    val sheetState = rememberModalBottomSheetState(skipPartiallyExpanded = true)
    var linkPassword by remember { mutableStateOf("") }
    var linkExpiry by remember { mutableStateOf("") }
    var username by remember { mutableStateOf("") }
    var revoking by remember { mutableStateOf<Share?>(null) }

    ModalBottomSheet(onDismissRequest = onDismiss, sheetState = sheetState) {
        Column(
            modifier = Modifier
                .heightIn(max = 620.dp)
                .verticalScroll(rememberScrollState())
                .padding(bottom = 28.dp),
        ) {
            Column(modifier = Modifier.padding(horizontal = 24.dp)) {
                Text("Share", style = MaterialTheme.typography.titleMedium)
                Spacer(Modifier.height(2.dp))
                Text(
                    text = row.name,
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                )
            }
            Spacer(Modifier.height(14.dp))
            HorizontalDivider()

            // --- who already has access ------------------------------------
            when {
                loading && shares.isEmpty() -> Box(
                    modifier = Modifier
                        .fillMaxWidth()
                        .height(90.dp),
                    contentAlignment = Alignment.Center,
                ) { CircularProgressIndicator() }

                error != null -> Row(
                    modifier = Modifier
                        .fillMaxWidth()
                        .padding(horizontal = 24.dp, vertical = 18.dp),
                    verticalAlignment = Alignment.Top,
                ) {
                    Icon(
                        Icons.Filled.WarningAmber,
                        contentDescription = null,
                        tint = MaterialTheme.colorScheme.error,
                        modifier = Modifier.size(20.dp),
                    )
                    Spacer(Modifier.width(12.dp))
                    Text(
                        text = error,
                        style = MaterialTheme.typography.bodyMedium,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }

                shares.isEmpty() -> Text(
                    text = "Not shared with anyone yet.",
                    style = MaterialTheme.typography.bodyMedium,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(horizontal = 24.dp, vertical = 18.dp),
                )

                else -> Column(modifier = Modifier.padding(top = 4.dp)) {
                    shares.forEach { share ->
                        ExistingShareRow(
                            share = share,
                            enabled = !busy,
                            onCopy = { onCopyLink(share) },
                            onRevoke = { revoking = share },
                        )
                    }
                }
            }

            HorizontalDivider()
            SheetError(actionError)
            Spacer(Modifier.height(16.dp))

            // --- give access ------------------------------------------------
            Column(modifier = Modifier.padding(horizontal = 24.dp)) {
                SectionLabel("Create a link")
                OutlinedTextField(
                    value = linkPassword,
                    onValueChange = { linkPassword = it },
                    modifier = Modifier.fillMaxWidth(),
                    singleLine = true,
                    label = { Text("Password (optional)") },
                    leadingIcon = { Icon(Icons.Filled.Lock, contentDescription = null) },
                    visualTransformation = PasswordVisualTransformation(),
                    keyboardOptions = KeyboardOptions(imeAction = ImeAction.Next),
                )
                Spacer(Modifier.height(8.dp))
                OutlinedTextField(
                    value = linkExpiry,
                    onValueChange = { linkExpiry = it },
                    modifier = Modifier.fillMaxWidth(),
                    singleLine = true,
                    label = { Text("Expires (YYYY-MM-DD, optional)") },
                    keyboardOptions = KeyboardOptions(imeAction = ImeAction.Done),
                )
                Spacer(Modifier.height(6.dp))
                // A link with no password is reachable by anyone who gets it,
                // forwarded or not. Say so where the decision is made.
                Text(
                    text = if (linkPassword.isBlank()) {
                        "Without a password, anyone who gets the link can open it."
                    } else {
                        "Only people who know the password can open it."
                    },
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
                Spacer(Modifier.height(10.dp))
                Button(
                    onClick = { onCreateLink(linkPassword, linkExpiry) },
                    enabled = !busy,
                    modifier = Modifier.fillMaxWidth(),
                ) {
                    Icon(Icons.Filled.Link, contentDescription = null, modifier = Modifier.size(18.dp))
                    Spacer(Modifier.width(8.dp))
                    Text("Create link")
                }

                Spacer(Modifier.height(22.dp))
                SectionLabel("Share with someone on this server")
                OutlinedTextField(
                    value = username,
                    onValueChange = { username = it },
                    modifier = Modifier.fillMaxWidth(),
                    singleLine = true,
                    label = { Text("Username") },
                    leadingIcon = { Icon(Icons.Filled.Person, contentDescription = null) },
                    keyboardOptions = KeyboardOptions(imeAction = ImeAction.Done),
                )
                Spacer(Modifier.height(10.dp))
                OutlinedButton(
                    onClick = { onShareWithUser(username); username = "" },
                    enabled = !busy && username.isNotBlank(),
                    modifier = Modifier.fillMaxWidth(),
                ) {
                    Icon(
                        Icons.Filled.PersonAdd,
                        contentDescription = null,
                        modifier = Modifier.size(18.dp),
                    )
                    Spacer(Modifier.width(8.dp))
                    Text("Share")
                }
                Spacer(Modifier.height(6.dp))
                Text(
                    text = "They get read-only access.",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
        }
    }

    revoking?.let { share ->
        AlertDialog(
            onDismissRequest = { revoking = null },
            title = { Text("Remove access?") },
            text = {
                Column {
                    Text(
                        if (share.isLink) "The link stops working. Anyone holding it loses access."
                        else "${share.recipientLabel} will no longer be able to open “${row.name}”."
                    )
                    Spacer(Modifier.height(8.dp))
                    // The distinction people actually worry about at this prompt.
                    Text(
                        "The file itself is not deleted.",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            },
            confirmButton = {
                TextButton(onClick = { revoking = null; onRevoke(share) }) {
                    Text("Remove access", color = MaterialTheme.colorScheme.error)
                }
            },
            dismissButton = { TextButton(onClick = { revoking = null }) { Text("Cancel") } },
        )
    }
}

@Composable
private fun SectionLabel(text: String) {
    Text(
        text = text,
        style = MaterialTheme.typography.labelLarge,
        color = MaterialTheme.colorScheme.onSurfaceVariant,
        modifier = Modifier.padding(bottom = 8.dp),
    )
}

@Composable
private fun ExistingShareRow(
    share: Share,
    enabled: Boolean,
    onCopy: () -> Unit,
    onRevoke: () -> Unit,
) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .padding(start = 24.dp, end = 8.dp, top = 10.dp, bottom = 10.dp),
        verticalAlignment = Alignment.CenterVertically,
        horizontalArrangement = Arrangement.SpaceBetween,
    ) {
        Icon(
            imageVector = shareIcon(share),
            contentDescription = share.kindLabel,
            modifier = Modifier.size(20.dp),
            tint = MaterialTheme.colorScheme.onSurfaceVariant,
        )
        Spacer(Modifier.width(14.dp))
        Column(modifier = Modifier.weight(1f)) {
            Text(
                text = share.recipientLabel,
                style = MaterialTheme.typography.bodyMedium,
                maxLines = 1,
                overflow = TextOverflow.Ellipsis,
            )
            if (share.hasExpiry) {
                Text(
                    text = "Expires ${share.expiration}",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
        }
        if (share.url.isNotBlank()) {
            IconButton(onClick = onCopy, enabled = enabled) {
                Icon(
                    Icons.Filled.ContentCopy,
                    contentDescription = "Copy this link",
                    tint = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
        }
        IconButton(onClick = onRevoke, enabled = enabled) {
            Icon(
                Icons.Filled.LinkOff,
                contentDescription = "Remove ${share.recipientLabel}'s access",
                tint = MaterialTheme.colorScheme.error,
            )
        }
    }
}

private fun shareIcon(share: Share): ImageVector = when (share.shareType) {
    Share.SHARE_TYPE_USER -> Icons.Filled.Person
    Share.SHARE_TYPE_GROUP, Share.SHARE_TYPE_CIRCLE -> Icons.Filled.Group
    Share.SHARE_TYPE_LINK -> Icons.Filled.Link
    Share.SHARE_TYPE_EMAIL -> Icons.Filled.Mail
    Share.SHARE_TYPE_FEDERATED, Share.SHARE_TYPE_FEDERATED_GROUP -> Icons.Filled.Public
    else -> Icons.Filled.Share
}
