/*
 * SharedScreen.kt — the two halves of sharing, kept apart.
 *
 * What you shared out and what was shared with you mean opposite things, and a
 * single merged list could not say which a row is. They also behave
 * differently: an own share names a path in this account and can be opened,
 * while a received share names a path in someone ELSE's account, which need not
 * exist here at all — so this screen never offers to open one.
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
import androidx.compose.material.icons.filled.ContentCopy
import androidx.compose.material.icons.filled.Folder
import androidx.compose.material.icons.filled.Group
import androidx.compose.material.icons.filled.InsertDriveFile
import androidx.compose.material.icons.filled.Link
import androidx.compose.material.icons.filled.Mail
import androidx.compose.material.icons.filled.Person
import androidx.compose.material.icons.filled.Public
import androidx.compose.material.icons.filled.Refresh
import androidx.compose.material.icons.filled.Schedule
import androidx.compose.material.icons.filled.Share
import androidx.compose.material.icons.filled.WarningAmber
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
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import dev.otherworld.nimbo.core.Share
import dev.otherworld.nimbo.core.SharesPayload

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun SharedScreen(
    shares: SharesPayload,
    loading: Boolean,
    error: String?,
    onOpenOwn: (Share) -> Unit,
    onCopyLink: (Share) -> Unit,
    onRefresh: () -> Unit,
    onBack: () -> Unit,
) {
    Scaffold(
        modifier = Modifier.fillMaxSize(),
        topBar = {
            Column {
                NimboTopBar(title = "Shared", onBack = onBack) {
                    IconButton(onClick = onRefresh) {
                        Icon(Icons.Filled.Refresh, contentDescription = "Refresh")
                    }
                }
                if (loading) LinearProgressIndicator(modifier = Modifier.fillMaxWidth())
            }
        },
    ) { inner ->
        val empty = shares.own.isEmpty() && shares.received.isEmpty()
        Box(
            modifier = Modifier
                .fillMaxSize()
                .padding(inner),
        ) {
            when {
                loading && empty -> Box(
                    modifier = Modifier.fillMaxSize(),
                    contentAlignment = Alignment.Center,
                ) { CircularProgressIndicator() }

                error != null -> EmptyState(
                    icon = Icons.Filled.WarningAmber,
                    title = "Couldn't load shares",
                    subtitle = error,
                )

                empty -> EmptyState(
                    icon = Icons.Filled.Share,
                    title = "Nothing shared",
                    subtitle = "Files you share, and files other people share with you, appear here.",
                )

                else -> LazyColumn(
                    modifier = Modifier.fillMaxSize(),
                    contentPadding = PaddingValues(bottom = 24.dp),
                ) {
                    if (shares.own.isNotEmpty()) {
                        item { SectionHeader("Shared by you") }
                        items(items = shares.own, key = { "own-${it.id}" }) { share ->
                            ShareRow(
                                share = share,
                                subtitle = ownSubtitle(share),
                                onClick = { onOpenOwn(share) },
                                trailing = {
                                    if (share.url.isNotBlank()) {
                                        IconButton(onClick = { onCopyLink(share) }) {
                                            Icon(
                                                Icons.Filled.ContentCopy,
                                                contentDescription = "Copy the link to ${share.name}",
                                                tint = MaterialTheme.colorScheme.onSurfaceVariant,
                                            )
                                        }
                                    }
                                },
                            )
                            HorizontalDivider()
                        }
                    }
                    if (shares.received.isNotEmpty()) {
                        item { SectionHeader("Shared with you") }
                        items(items = shares.received, key = { "in-${it.id}" }) { share ->
                            // No onClick: the path belongs to the owner's account.
                            ShareRow(
                                share = share,
                                subtitle = "from ${share.sharedBy}",
                                onClick = null,
                                trailing = {},
                            )
                            HorizontalDivider()
                        }
                    }
                }
            }
        }
    }
}

/** What an own share says under its name: who has it, and whether it expires. */
private fun ownSubtitle(share: Share): String = buildString {
    append(share.recipientLabel)
    if (share.hasExpiry) {
        append(" · expires ")
        append(share.expiration)
    }
}

@Composable
private fun ShareRow(
    share: Share,
    subtitle: String,
    onClick: (() -> Unit)?,
    trailing: @Composable () -> Unit,
) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .then(if (onClick != null) Modifier.clickable(onClick = onClick) else Modifier)
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
                imageVector = if (share.isFolder) Icons.Filled.Folder else Icons.Filled.InsertDriveFile,
                contentDescription = null,
                modifier = Modifier.size(22.dp),
                tint = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
        Spacer(Modifier.width(14.dp))
        Column(modifier = Modifier.weight(1f)) {
            Text(
                text = share.name,
                style = MaterialTheme.typography.bodyLarge,
                maxLines = 1,
                overflow = TextOverflow.Ellipsis,
            )
            Row(verticalAlignment = Alignment.CenterVertically) {
                Icon(
                    imageVector = shareTypeIcon(share),
                    contentDescription = share.kindLabel,
                    modifier = Modifier.size(14.dp),
                    tint = MaterialTheme.colorScheme.onSurfaceVariant,
                )
                Spacer(Modifier.width(5.dp))
                Text(
                    text = subtitle,
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                )
            }
        }
        if (share.hasExpiry) {
            Icon(
                Icons.Filled.Schedule,
                contentDescription = "Expires ${share.expiration}",
                modifier = Modifier.size(16.dp),
                tint = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            Spacer(Modifier.width(4.dp))
        }
        trailing()
    }
}

private fun shareTypeIcon(share: Share): ImageVector = when (share.shareType) {
    Share.SHARE_TYPE_USER -> Icons.Filled.Person
    Share.SHARE_TYPE_GROUP, Share.SHARE_TYPE_CIRCLE -> Icons.Filled.Group
    Share.SHARE_TYPE_LINK -> Icons.Filled.Link
    Share.SHARE_TYPE_EMAIL -> Icons.Filled.Mail
    Share.SHARE_TYPE_FEDERATED, Share.SHARE_TYPE_FEDERATED_GROUP -> Icons.Filled.Public
    else -> Icons.Filled.Share
}
