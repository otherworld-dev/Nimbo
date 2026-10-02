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
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import dev.otherworld.nimbo.supporter.SupporterNotice
import dev.otherworld.nimbo.supporter.SupporterTier

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun SupportScreen(
    tier: SupporterTier,
    notice: SupporterNotice?,
    onDismissNotice: () -> Unit,
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
        }
    }
}

