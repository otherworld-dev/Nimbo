/*
 * TabHost.kt — the bottom-nav shell: Files / Sync / Apps.
 *
 * Files is the app's front door; Sync keeps the status, folders and controls one
 * tap away and wears a dot while a transfer is running, so the thing Nimbo is
 * actually for stays visible without occupying the main screen.
 */
package dev.otherworld.nimbo.ui

import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Apps
import androidx.compose.material.icons.filled.Folder
import androidx.compose.material.icons.filled.Sync
import androidx.compose.material3.Badge
import androidx.compose.material3.BadgedBox
import androidx.compose.material3.Icon
import androidx.compose.material3.NavigationBar
import androidx.compose.material3.NavigationBarItem
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Modifier

@Composable
fun TabHost(
    tab: Tab,
    onSelectTab: (Tab) -> Unit,
    syncing: Boolean,
    content: @Composable () -> Unit,
) {
    Scaffold(
        modifier = Modifier.fillMaxSize(),
        bottomBar = {
            NavigationBar {
                NavigationBarItem(
                    selected = tab == Tab.FILES,
                    onClick = { onSelectTab(Tab.FILES) },
                    icon = { Icon(Icons.Filled.Folder, contentDescription = null) },
                    label = { Text("Files") },
                )
                NavigationBarItem(
                    selected = tab == Tab.SYNC,
                    onClick = { onSelectTab(Tab.SYNC) },
                    icon = {
                        // A dot while transfers are running: the reason to look at
                        // the Sync tab is precisely that something is happening there.
                        BadgedBox(badge = { if (syncing) Badge() }) {
                            Icon(Icons.Filled.Sync, contentDescription = null)
                        }
                    },
                    label = { Text("Sync") },
                )
                NavigationBarItem(
                    selected = tab == Tab.APPS,
                    onClick = { onSelectTab(Tab.APPS) },
                    icon = { Icon(Icons.Filled.Apps, contentDescription = null) },
                    label = { Text("Apps") },
                )
            }
        },
    ) { inner ->
        // Each tab's screen carries its own top bar and insets; only the bottom
        // bar's space is reserved here.
        Box(
            modifier = Modifier
                .fillMaxSize()
                .padding(bottom = inner.calculateBottomPadding()),
        ) {
            content()
        }
    }
}
