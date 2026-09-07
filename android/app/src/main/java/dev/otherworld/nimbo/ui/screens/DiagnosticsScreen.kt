/*
 * DiagnosticsScreen.kt — stateless read-out of the engine's Diagnostics payload:
 * server identity, push channel health and the last sync/status strings.
 */
package dev.otherworld.nimbo.ui.screens

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
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Info
import androidx.compose.material.icons.filled.Refresh
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import dev.otherworld.nimbo.core.Diagnostics

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun DiagnosticsScreen(
    diagnostics: Diagnostics?,
    onRefresh: () -> Unit,
    onBack: () -> Unit,
) {
    Scaffold(
        modifier = Modifier.fillMaxSize(),
        topBar = {
            NimboTopBar(title = "Diagnostics", onBack = onBack) {
                IconButton(onClick = onRefresh) {
                    Icon(Icons.Filled.Refresh, contentDescription = "Refresh")
                }
            }
        },
    ) { inner ->
        if (diagnostics == null) {
            Box(
                modifier = Modifier
                    .fillMaxSize()
                    .padding(inner),
                contentAlignment = Alignment.Center,
            ) {
                EmptyState(
                    icon = Icons.Filled.Info,
                    title = "Nothing collected yet",
                    subtitle = "Diagnostics are read from the running engine.",
                    action = {
                        Button(onClick = onRefresh, shape = RoundedCornerShape(14.dp)) {
                            Icon(
                                Icons.Filled.Refresh,
                                contentDescription = null,
                                modifier = Modifier.size(18.dp),
                            )
                            Spacer(Modifier.width(8.dp))
                            Text("Refresh")
                        }
                    },
                )
            }
        } else {
            val scroll = rememberScrollState()
            Column(
                modifier = Modifier
                    .fillMaxSize()
                    .padding(inner)
                    .verticalScroll(scroll)
                    .padding(PaddingValues(start = 20.dp, end = 20.dp, top = 16.dp, bottom = 32.dp)),
            ) {
                SectionHeader("Server")
                Spacer(Modifier.height(8.dp))
                InfoCard {
                    LabelValueRow("Server URL", diagnostics.serverURL)
                    HorizontalDivider()
                    LabelValueRow("Server version", diagnostics.serverVersion)
                    HorizontalDivider()
                    LabelValueRow("Account", diagnostics.account)
                }

                Spacer(Modifier.height(24.dp))
                SectionHeader("Push channel")
                Spacer(Modifier.height(8.dp))
                InfoCard {
                    Row(
                        modifier = Modifier
                            .fillMaxWidth()
                            .padding(vertical = 10.dp),
                        horizontalArrangement = Arrangement.spacedBy(8.dp),
                        verticalAlignment = Alignment.CenterVertically,
                    ) {
                        StatPill(
                            text = if (diagnostics.pushAvailable) "Available" else "Unavailable",
                            container = if (diagnostics.pushAvailable)
                                MaterialTheme.colorScheme.secondaryContainer
                            else MaterialTheme.colorScheme.surfaceVariant,
                            content = if (diagnostics.pushAvailable)
                                MaterialTheme.colorScheme.onSecondaryContainer
                            else MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                        StatPill(
                            text = if (diagnostics.pushConnected) "Connected" else "Not connected",
                            container = if (diagnostics.pushConnected)
                                MaterialTheme.colorScheme.primaryContainer
                            else MaterialTheme.colorScheme.surfaceVariant,
                            content = if (diagnostics.pushConnected)
                                MaterialTheme.colorScheme.onPrimaryContainer
                            else MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                    }
                    HorizontalDivider()
                    LabelValueRow("Connected since", stamp(diagnostics.pushSince))
                }

                Spacer(Modifier.height(24.dp))
                SectionHeader("Sync")
                Spacer(Modifier.height(8.dp))
                InfoCard {
                    LabelValueRow("Last status", diagnostics.lastStatus)
                    HorizontalDivider()
                    LabelValueRow("Last sync at", stamp(diagnostics.lastSyncAt))
                }

                Spacer(Modifier.height(24.dp))
                Text(
                    text = "Values are read straight from the sync engine. Tap refresh " +
                        "after changing anything on the server.",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
        }
    }
}

/**
 * Go's untagged `time.Time` fields serialise as RFC 3339, and a zero time comes
 * across as "0001-01-01T00:00:00Z" — which is a value, not a blank, so it would
 * otherwise be printed verbatim to the user.
 */
private fun stamp(value: String): String =
    if (value.isBlank() || value.startsWith("0001-01-01")) "Never" else value

@Composable
private fun InfoCard(content: @Composable androidx.compose.foundation.layout.ColumnScope.() -> Unit) {
    Card(
        modifier = Modifier.fillMaxWidth(),
        shape = RoundedCornerShape(18.dp),
        colors = CardDefaults.cardColors(
            containerColor = MaterialTheme.colorScheme.surfaceVariant.copy(alpha = 0.45f),
        ),
    ) {
        Column(modifier = Modifier.padding(horizontal = 18.dp, vertical = 8.dp)) {
            content()
        }
    }
}
