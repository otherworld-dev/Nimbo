/*
 * SupportScreen.kt — "Support Nimbo". The status line and footer are shared;
 * the middle, how you pay, is the flavour's SupportActions(), passed in.
 */
package dev.otherworld.nimbo.ui.screens

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.selection.selectable
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.RadioButton
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.unit.dp
import dev.otherworld.nimbo.supporter.SupporterNotice
import dev.otherworld.nimbo.supporter.SupporterTier

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun SupportScreen(
    tier: SupporterTier,
    notice: SupporterNotice?,
    debugOverride: SupporterTier?,
    onDismissNotice: () -> Unit,
    /** Null in release builds: the override section does not exist there. */
    onDebugOverride: ((SupporterTier?) -> Unit)?,
    onOpenBusiness: () -> Unit,
    onBack: () -> Unit,
    actions: @Composable () -> Unit,
) {
    Scaffold(
        modifier = Modifier.fillMaxSize(),
        topBar = { NimboTopBar(title = "Support Nimbo", onBack = onBack) },
    ) { inner ->
        Column(
            modifier = Modifier
                .fillMaxSize()
                .padding(inner)
                .verticalScroll(rememberScrollState())
                .padding(horizontal = 20.dp, vertical = 16.dp),
            verticalArrangement = Arrangement.spacedBy(12.dp),
        ) {
            if (tier == SupporterTier.NONE) {
                Text(
                    "Nimbo is free, with every feature. If it's useful to you, you can support it.",
                    style = MaterialTheme.typography.bodyLarge,
                )
            } else {
                Row(verticalAlignment = Alignment.CenterVertically) {
                    SupporterBadge(tier)
                    Spacer(Modifier.width(10.dp))
                    Text(
                        if (tier == SupporterTier.ONE_OFF) "You've supported Nimbo. Thank you."
                        else "You're a ${tier.label}. Thank you.",
                        style = MaterialTheme.typography.bodyLarge,
                    )
                }
            }

            SupportNoticeCard(notice, onDismissNotice)

            actions()

            HorizontalDivider()
            TextButton(onClick = onOpenBusiness) {
                Text("Using Nimbo at work? Businesses need a commercial licence")
            }

            if (onDebugOverride != null) {
                DebugTierOverride(debugOverride, onDebugOverride)
            }
        }
    }
}

/** Debug builds only: see every perk and screen without paying. */
@Composable
private fun DebugTierOverride(current: SupporterTier?, onChange: (SupporterTier?) -> Unit) {
    SectionHeader("Debug: pretend tier")
    val options: List<SupporterTier?> = listOf(null) + SupporterTier.entries
    options.forEach { option ->
        Row(
            modifier = Modifier
                .fillMaxWidth()
                .selectable(selected = option == current, role = Role.RadioButton, onClick = { onChange(option) })
                .padding(vertical = 6.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            RadioButton(selected = option == current, onClick = null)
            Spacer(Modifier.width(8.dp))
            Text(
                when (option) {
                    null -> "Real status"
                    SupporterTier.NONE -> "Not a supporter"
                    SupporterTier.ONE_OFF -> "One-off"
                    else -> option.label
                },
            )
        }
    }
}
