/*
 * SettingsScreen.kt — the app's own preferences, as opposed to the account's.
 *
 * Appearance, plus the way in to supporting Nimbo. Everything else Nimbo does is decided by the sync
 * engine or by the server, and inventing settings for their own sake gives
 * people more ways to break something than to fix it.
 */
package dev.otherworld.nimbo.ui.screens

import androidx.compose.foundation.Image
import androidx.compose.foundation.clickable
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
import androidx.compose.foundation.selection.selectable
import androidx.compose.foundation.selection.selectableGroup
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Circle
import androidx.compose.material.icons.filled.Favorite
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.RadioButton
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.res.painterResource
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.unit.dp
import androidx.compose.ui.graphics.Color
import dev.otherworld.nimbo.R
import dev.otherworld.nimbo.supporter.AccentChoice
import dev.otherworld.nimbo.supporter.AppIcon
import dev.otherworld.nimbo.supporter.SupporterTier
import dev.otherworld.nimbo.supporter.SupporterUi
import dev.otherworld.nimbo.ui.theme.AppearancePreference
import dev.otherworld.nimbo.ui.theme.parseThemeColor

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun SettingsScreen(
    appearance: AppearancePreference,
    serverAppearance: String,
    themeColor: String,
    supporter: SupporterUi,
    onAppearanceChange: (AppearancePreference) -> Unit,
    onIconChange: (AppIcon) -> Unit,
    onAccentChange: (AccentChoice) -> Unit,
    onOpenSupport: () -> Unit,
    onBack: () -> Unit,
) {
    Scaffold(
        modifier = Modifier.fillMaxSize(),
        topBar = { NimboTopBar(title = "Settings", onBack = onBack) },
    ) { inner ->
        Column(
            modifier = Modifier
                .fillMaxSize()
                .padding(inner)
                .verticalScroll(rememberScrollState()),
        ) {
            SupportRow(tier = supporter.tier, onClick = onOpenSupport)
            HorizontalDivider(modifier = Modifier.padding(vertical = 8.dp))
            SectionHeader("Appearance")
            Column(modifier = Modifier.selectableGroup()) {
                AppearancePreference.entries.forEach { option ->
                    AppearanceRow(
                        option = option,
                        selected = option == appearance,
                        subtitle = subtitleFor(option, serverAppearance),
                        onSelect = { onAppearanceChange(option) },
                    )
                }
            }

            HorizontalDivider(modifier = Modifier.padding(vertical = 8.dp))
            SectionHeader("Colour")
            AccentRow(themeColor)
            if (supporter.perks.accents) {
                Column(modifier = Modifier.selectableGroup()) {
                    AccentChoice.entries.forEach { choice ->
                        AccentChoiceRow(
                            choice = choice,
                            selected = choice == supporter.accent,
                            onSelect = { onAccentChange(choice) },
                        )
                    }
                }
            } else {
                LockedPerkRow("Accent colours", SupporterTier.PATRON, onOpenSupport)
            }

            HorizontalDivider(modifier = Modifier.padding(vertical = 8.dp))
            SectionHeader("App icon")
            if (supporter.perks.icons) {
                Column(modifier = Modifier.selectableGroup()) {
                    AppIcon.entries.forEach { icon ->
                        IconRow(icon = icon, selected = icon == supporter.icon, onSelect = { onIconChange(icon) })
                    }
                }
                Text(
                    "Your home-screen shortcut may need re-adding after a change.",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(horizontal = 16.dp, vertical = 8.dp),
                )
            } else {
                LockedPerkRow("Alternative app icons", SupporterTier.BACKER, onOpenSupport)
            }
        }
    }
}

/**
 * What each option will actually do right now — the useful half of the choice.
 * "Follow Nextcloud" in particular does something different depending on a
 * setting the user made somewhere else entirely, so it says which.
 */
private fun subtitleFor(option: AppearancePreference, serverAppearance: String): String =
    when (option) {
        AppearancePreference.FOLLOW_NEXTCLOUD -> when (serverAppearance.trim().lowercase()) {
            "dark" -> "Your Nextcloud is set to dark"
            "light" -> "Your Nextcloud is set to light"
            "" -> "Your Nextcloud hasn't said yet — the phone decides"
            else -> "Your Nextcloud follows its own system, so the phone decides"
        }
        AppearancePreference.FOLLOW_SYSTEM -> "Matches your phone's dark mode"
        AppearancePreference.ALWAYS_DARK -> "Dark, whatever the phone or server says"
        AppearancePreference.ALWAYS_LIGHT -> "Light, whatever the phone or server says"
    }

@Composable
private fun AppearanceRow(
    option: AppearancePreference,
    selected: Boolean,
    subtitle: String,
    onSelect: () -> Unit,
) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .selectable(selected = selected, role = Role.RadioButton, onClick = onSelect)
            .padding(horizontal = 12.dp, vertical = 12.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        RadioButton(selected = selected, onClick = null)
        Spacer(Modifier.width(8.dp))
        Column {
            Text(option.label, style = MaterialTheme.typography.bodyLarge)
            Text(
                text = subtitle,
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
    }
}

/**
 * The accent is the Nextcloud colour unless a Patron picks one below.
 * This row exists to show the current base, because a colour arriving from
 * somewhere else is otherwise a mystery.
 */
@Composable
private fun AccentRow(themeColor: String) {
    val parsed = parseThemeColor(themeColor)
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .padding(horizontal = 16.dp, vertical = 12.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Box(
            modifier = Modifier
                .size(28.dp)
                .clip(CircleShape),
        ) {
            Icon(
                imageVector = Icons.Filled.Circle,
                contentDescription = null,
                tint = parsed ?: MaterialTheme.colorScheme.primary,
                modifier = Modifier.size(28.dp),
            )
        }
        Spacer(Modifier.width(14.dp))
        Column(modifier = Modifier.weight(1f)) {
            Text(
                text = if (parsed != null) "From your Nextcloud theme" else "Nimbo's own colour",
                style = MaterialTheme.typography.bodyLarge,
            )
            Text(
                text = when {
                    parsed != null -> themeColor.trim().uppercase()
                    else -> "Your server hasn't given one, so the app uses its own"
                },
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
    }
    Spacer(Modifier.height(16.dp))
}

/** First in Settings: the way in to supporting, and where the badge lives. */
@Composable
private fun SupportRow(tier: SupporterTier, onClick: () -> Unit) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .clickable(onClick = onClick)
            .padding(horizontal = 16.dp, vertical = 14.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Icon(
            Icons.Filled.Favorite,
            contentDescription = null,
            tint = MaterialTheme.colorScheme.primary,
            modifier = Modifier.size(24.dp),
        )
        Spacer(Modifier.width(14.dp))
        Column(modifier = Modifier.weight(1f)) {
            Text("Support Nimbo", style = MaterialTheme.typography.bodyLarge)
            Text(
                if (tier == SupporterTier.NONE) "Nimbo is free. Support it if it's useful to you"
                else "Thank you for supporting Nimbo",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
        SupporterBadge(tier)
    }
}

@Composable
private fun IconRow(icon: AppIcon, selected: Boolean, onSelect: () -> Unit) {
    val (background, foreground) = when (icon) {
        AppIcon.DEFAULT -> R.drawable.ic_launcher_background to R.drawable.ic_launcher_foreground
        AppIcon.FOREST -> R.drawable.ic_launcher_forest_background to R.drawable.ic_launcher_forest_foreground
        AppIcon.EMBER -> R.drawable.ic_launcher_ember_background to R.drawable.ic_launcher_ember_foreground
        AppIcon.SLATE -> R.drawable.ic_launcher_slate_background to R.drawable.ic_launcher_slate_foreground
    }
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .selectable(selected = selected, role = Role.RadioButton, onClick = onSelect)
            .padding(horizontal = 12.dp, vertical = 8.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        RadioButton(selected = selected, onClick = null)
        Spacer(Modifier.width(8.dp))
        // The adaptive icon's two layers, stacked: an <adaptive-icon> itself
        // can't be loaded by painterResource.
        Box(modifier = Modifier.size(40.dp).clip(CircleShape)) {
            Image(painterResource(background), contentDescription = null, modifier = Modifier.fillMaxSize())
            Image(painterResource(foreground), contentDescription = null, modifier = Modifier.fillMaxSize())
        }
        Spacer(Modifier.width(12.dp))
        Text(icon.label, style = MaterialTheme.typography.bodyLarge)
    }
}

@Composable
private fun AccentChoiceRow(choice: AccentChoice, selected: Boolean, onSelect: () -> Unit) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .selectable(selected = selected, role = Role.RadioButton, onClick = onSelect)
            .padding(horizontal = 12.dp, vertical = 10.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        RadioButton(selected = selected, onClick = null)
        Spacer(Modifier.width(8.dp))
        Icon(
            imageVector = Icons.Filled.Circle,
            contentDescription = null,
            tint = choice.argb?.let { Color(it) } ?: MaterialTheme.colorScheme.outline,
            modifier = Modifier.size(24.dp),
        )
        Spacer(Modifier.width(12.dp))
        Text(choice.label, style = MaterialTheme.typography.bodyLarge)
    }
}
