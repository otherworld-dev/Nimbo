/*
 * AppsScreen.kt — stateless grid of the Nextcloud apps the account can reach.
 * Icons are remote SVGs and there is no image loader in the pinned dependency
 * set, so each tile shows the app's initial in a coloured circle instead.
 */
package dev.otherworld.nimbo.ui.screens

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.grid.GridCells
import androidx.compose.foundation.lazy.grid.LazyVerticalGrid
import androidx.compose.foundation.lazy.grid.items
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Apps
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import dev.otherworld.nimbo.core.NcApp

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun AppsScreen(
    apps: List<NcApp>,
    onOpen: (NcApp) -> Unit,
    onOpenNotifications: () -> Unit,
    notificationCount: Int,
    onBack: (() -> Unit)? = null,
) {
    Scaffold(
        modifier = Modifier.fillMaxSize(),
        topBar = {
            NimboTopBar(title = "Nextcloud apps", onBack = onBack) {
                NotificationBell(notificationCount, onOpenNotifications)
            }
        },
    ) { inner ->
        if (apps.isEmpty()) {
            Box(
                modifier = Modifier
                    .fillMaxSize()
                    .padding(inner),
                contentAlignment = Alignment.Center,
            ) {
                EmptyState(
                    icon = Icons.Filled.Apps,
                    title = "No apps to show",
                    subtitle = "Once Nimbo has talked to your server, the apps it " +
                        "offers appear here.",
                )
            }
        } else {
            LazyVerticalGrid(
                columns = GridCells.Adaptive(96.dp),
                modifier = Modifier
                    .fillMaxSize()
                    .padding(inner),
                contentPadding = PaddingValues(
                    start = 16.dp,
                    end = 16.dp,
                    top = 16.dp,
                    bottom = 32.dp,
                ),
                horizontalArrangement = Arrangement.spacedBy(8.dp),
                verticalArrangement = Arrangement.spacedBy(8.dp),
            ) {
                items(items = apps, key = { it.id.ifBlank { it.name } }) { app ->
                    AppTile(app = app, onOpen = onOpen)
                }
            }
        }
    }
}

@Composable
private fun AppTile(app: NcApp, onOpen: (NcApp) -> Unit) {
    val label = app.name.ifBlank { app.id }
    Column(
        modifier = Modifier
            .fillMaxWidth()
            .clip(RoundedCornerShape(18.dp))
            .clickable { onOpen(app) }
            .padding(vertical = 14.dp, horizontal = 6.dp),
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        InitialCircle(name = label, diameter = 56.dp)
        Spacer(Modifier.height(10.dp))
        Text(
            text = label,
            style = MaterialTheme.typography.labelLarge,
            color = MaterialTheme.colorScheme.onSurface,
            textAlign = TextAlign.Center,
            maxLines = 2,
            overflow = TextOverflow.Ellipsis,
        )
    }
}
