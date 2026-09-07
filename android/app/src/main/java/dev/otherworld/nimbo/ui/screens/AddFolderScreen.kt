/*
 * AddFolderScreen.kt — the confirmation step between picking a remote folder
 * and creating the sync pair. Stateless: it shows the two ends of the pair,
 * offers to change the local one, and reports back through onConfirm.
 *
 * The whole point of the screen is that a pair has two ends and syncs both
 * ways, so the layout says exactly that: server card, two-way link, phone card.
 */
package dev.otherworld.nimbo.ui.screens

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.CloudQueue
import androidx.compose.material.icons.filled.PhoneAndroid
import androidx.compose.material.icons.filled.SwapVert
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import dev.otherworld.nimbo.platform.LocalFs

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun AddFolderScreen(
    remoteRoot: String,
    localDir: String,
    busy: Boolean,
    onChangeLocal: () -> Unit,
    onConfirm: () -> Unit,
    onBack: () -> Unit,
) {
    val remoteLabel = remember(remoteRoot) {
        remoteRoot.trim().trim('/').ifEmpty { "All files" }
    }
    val localLabel = remember(localDir) { LocalFs.displayPath(localDir) }
    val scroll = rememberScrollState()

    Scaffold(
        modifier = Modifier.fillMaxSize(),
        topBar = { NimboTopBar(title = "Add folder", onBack = onBack) },
        bottomBar = {
            Surface(tonalElevation = 3.dp, modifier = Modifier.fillMaxWidth()) {
                Column(
                    modifier = Modifier
                        .navigationBarsPadding()
                        .padding(horizontal = 20.dp, vertical = 16.dp)
                ) {
                    Button(
                        onClick = onConfirm,
                        enabled = !busy,
                        shape = RoundedCornerShape(14.dp),
                        modifier = Modifier
                            .fillMaxWidth()
                            .height(50.dp),
                    ) {
                        if (busy) {
                            CircularProgressIndicator(
                                strokeWidth = 2.dp,
                                color = MaterialTheme.colorScheme.onPrimary,
                                modifier = Modifier.size(18.dp),
                            )
                            Spacer(Modifier.width(10.dp))
                        }
                        Text(
                            text = if (busy) "Adding…" else "Add folder",
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
                .padding(inner)
                .verticalScroll(scroll)
                .padding(horizontal = 20.dp, vertical = 16.dp),
        ) {
            SectionHeader("This pair")
            Spacer(Modifier.height(8.dp))

            EndpointCard(
                icon = Icons.Filled.CloudQueue,
                label = "Nextcloud folder",
                value = remoteLabel,
                iconContainer = MaterialTheme.colorScheme.primaryContainer,
                iconTint = MaterialTheme.colorScheme.onPrimaryContainer,
            )

            TwoWayLink()

            EndpointCard(
                icon = Icons.Filled.PhoneAndroid,
                label = "On this phone",
                value = localLabel,
                iconContainer = MaterialTheme.colorScheme.secondaryContainer,
                iconTint = MaterialTheme.colorScheme.onSecondaryContainer,
                action = {
                    TextButton(onClick = onChangeLocal, enabled = !busy) {
                        Text("Change")
                    }
                },
            )

            Spacer(Modifier.height(18.dp))
            Text(
                text = "Files sync both ways. Anything you add, edit or delete in " +
                    "one folder happens in the other.",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            Spacer(Modifier.height(8.dp))
        }
    }
}

@Composable
private fun EndpointCard(
    icon: ImageVector,
    label: String,
    value: String,
    iconContainer: Color,
    iconTint: Color,
    action: @Composable (() -> Unit)? = null,
) {
    Card(
        modifier = Modifier.fillMaxWidth(),
        shape = RoundedCornerShape(18.dp),
        colors = CardDefaults.cardColors(
            containerColor = MaterialTheme.colorScheme.surfaceVariant.copy(alpha = 0.45f),
        ),
    ) {
        Row(
            modifier = Modifier.padding(
                start = 16.dp,
                top = 14.dp,
                bottom = 14.dp,
                end = if (action == null) 16.dp else 6.dp,
            ),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Box(
                modifier = Modifier
                    .size(42.dp)
                    .clip(CircleShape)
                    .background(iconContainer),
                contentAlignment = Alignment.Center,
            ) {
                Icon(
                    imageVector = icon,
                    contentDescription = null,
                    tint = iconTint,
                    modifier = Modifier.size(22.dp),
                )
            }
            Spacer(Modifier.width(14.dp))
            Column(modifier = Modifier.weight(1f)) {
                Text(
                    text = label,
                    style = MaterialTheme.typography.labelMedium,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
                Spacer(Modifier.height(3.dp))
                Text(
                    text = value,
                    style = MaterialTheme.typography.titleSmall,
                    fontWeight = FontWeight.SemiBold,
                    maxLines = 2,
                    overflow = TextOverflow.Ellipsis,
                )
            }
            if (action != null) {
                Spacer(Modifier.width(6.dp))
                action()
            }
        }
    }
}

/** The bit between the two cards that says "these mirror each other". */
@Composable
private fun TwoWayLink() {
    Row(
        modifier = Modifier.fillMaxWidth(),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Column(
            modifier = Modifier.width(58.dp),
            horizontalAlignment = Alignment.CenterHorizontally,
        ) {
            Connector()
            Box(
                modifier = Modifier
                    .size(32.dp)
                    .clip(CircleShape)
                    .background(MaterialTheme.colorScheme.surfaceVariant),
                contentAlignment = Alignment.Center,
            ) {
                Icon(
                    imageVector = Icons.Filled.SwapVert,
                    contentDescription = null,
                    tint = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.size(18.dp),
                )
            }
            Connector()
        }
        Spacer(Modifier.width(8.dp))
        Text(
            text = "Kept in step, both ways",
            style = MaterialTheme.typography.labelMedium,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }
}

@Composable
private fun Connector() {
    Box(
        modifier = Modifier
            .padding(vertical = 2.dp)
            .width(2.dp)
            .height(10.dp)
            .clip(RoundedCornerShape(1.dp))
            .background(MaterialTheme.colorScheme.outlineVariant),
    )
}
