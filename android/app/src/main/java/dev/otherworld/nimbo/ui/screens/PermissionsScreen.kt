/*
 * PermissionsScreen.kt — stateless setup screen listing the three OS grants the
 * sync engine needs, with per-row Grant buttons and a gated Continue action.
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
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.BatteryFull
import androidx.compose.material.icons.filled.CheckCircle
import androidx.compose.material.icons.filled.Folder
import androidx.compose.material.icons.filled.Notifications
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.FilledTonalButton
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp

@Composable
fun PermissionsScreen(
    hasAllFiles: Boolean,
    hasNotifications: Boolean,
    ignoringBattery: Boolean,
    onGrantAllFiles: () -> Unit,
    onGrantNotifications: () -> Unit,
    onGrantBattery: () -> Unit,
    onContinue: () -> Unit,
) {
    val scroll = rememberScrollState()

    Surface(
        modifier = Modifier.fillMaxSize(),
        color = MaterialTheme.colorScheme.background,
    ) {
        Column(modifier = Modifier.fillMaxSize()) {
            Column(
                modifier = Modifier
                    .weight(1f)
                    .verticalScroll(scroll)
                    .padding(horizontal = 24.dp),
            ) {
                Spacer(Modifier.height(48.dp))
                Text(
                    text = "Almost there",
                    style = MaterialTheme.typography.headlineSmall,
                    fontWeight = FontWeight.Bold,
                )
                Spacer(Modifier.height(8.dp))
                Text(
                    text = "Nimbo needs a few Android permissions before it can " +
                        "keep folders in sync in the background.",
                    style = MaterialTheme.typography.bodyMedium,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )

                Spacer(Modifier.height(28.dp))

                PermissionRow(
                    icon = Icons.Filled.Folder,
                    title = "All files access",
                    why = "Lets Nimbo read and write the sync folder in your shared storage.",
                    requirement = "Required",
                    granted = hasAllFiles,
                    onGrant = onGrantAllFiles,
                )
                Spacer(Modifier.height(14.dp))
                PermissionRow(
                    icon = Icons.Filled.Notifications,
                    title = "Notifications",
                    why = "Shows sync progress and tells you when something needs attention.",
                    requirement = "Recommended",
                    granted = hasNotifications,
                    onGrant = onGrantNotifications,
                )
                Spacer(Modifier.height(14.dp))
                PermissionRow(
                    icon = Icons.Filled.BatteryFull,
                    title = "Ignore battery optimisation",
                    why = "Stops Android suspending sync while the screen is off.",
                    requirement = "Recommended",
                    granted = ignoringBattery,
                    onGrant = onGrantBattery,
                )

                Spacer(Modifier.height(32.dp))
            }

            Surface(
                color = MaterialTheme.colorScheme.surface,
                tonalElevation = 3.dp,
                modifier = Modifier.fillMaxWidth(),
            ) {
                Column(modifier = Modifier.padding(horizontal = 24.dp, vertical = 18.dp)) {
                    if (!hasAllFiles) {
                        Text(
                            text = "All files access is required to continue.",
                            style = MaterialTheme.typography.bodySmall,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                        Spacer(Modifier.height(10.dp))
                    }
                    Button(
                        onClick = onContinue,
                        enabled = hasAllFiles,
                        shape = RoundedCornerShape(14.dp),
                        modifier = Modifier
                            .fillMaxWidth()
                            .height(52.dp),
                    ) {
                        Text(
                            text = "Continue",
                            style = MaterialTheme.typography.titleSmall,
                            fontWeight = FontWeight.SemiBold,
                        )
                    }
                }
            }
        }
    }
}

@Composable
private fun PermissionRow(
    icon: ImageVector,
    title: String,
    why: String,
    requirement: String,
    granted: Boolean,
    onGrant: () -> Unit,
) {
    Card(
        modifier = Modifier.fillMaxWidth(),
        shape = RoundedCornerShape(18.dp),
        colors = CardDefaults.cardColors(
            containerColor = MaterialTheme.colorScheme.surfaceVariant.copy(alpha = 0.45f),
        ),
    ) {
        Row(
            modifier = Modifier.padding(18.dp),
            verticalAlignment = Alignment.Top,
        ) {
            Box(
                modifier = Modifier
                    .size(42.dp)
                    .clip(CircleShape)
                    .background(
                        if (granted) MaterialTheme.colorScheme.primaryContainer
                        else MaterialTheme.colorScheme.surface
                    ),
                contentAlignment = Alignment.Center,
            ) {
                Icon(
                    imageVector = if (granted) Icons.Filled.CheckCircle else icon,
                    contentDescription = null,
                    tint = if (granted) MaterialTheme.colorScheme.onPrimaryContainer
                    else MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.size(22.dp),
                )
            }
            Spacer(Modifier.width(14.dp))
            Column(modifier = Modifier.weight(1f)) {
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Text(
                        text = title,
                        style = MaterialTheme.typography.titleSmall,
                        fontWeight = FontWeight.SemiBold,
                    )
                    Spacer(Modifier.width(8.dp))
                    Text(
                        text = requirement,
                        style = MaterialTheme.typography.labelSmall,
                        color = if (requirement == "Required")
                            MaterialTheme.colorScheme.primary
                        else MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
                Spacer(Modifier.height(4.dp))
                Text(
                    text = why,
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
                if (!granted) {
                    Spacer(Modifier.height(12.dp))
                    FilledTonalButton(
                        onClick = onGrant,
                        shape = RoundedCornerShape(12.dp),
                    ) {
                        Text("Grant")
                    }
                } else {
                    Spacer(Modifier.height(8.dp))
                    Text(
                        text = "Granted",
                        style = MaterialTheme.typography.labelMedium,
                        color = MaterialTheme.colorScheme.primary,
                        fontWeight = FontWeight.SemiBold,
                    )
                }
            }
        }
    }
}
