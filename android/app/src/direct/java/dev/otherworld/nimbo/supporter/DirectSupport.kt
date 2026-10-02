/*
 * DirectSupport.kt — the direct build's half of the supporter seam: the
 * repository factory and the "how you pay" part of the Support screen.
 */
package dev.otherworld.nimbo.supporter

import android.content.Context
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.Button
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import dev.otherworld.nimbo.LocalNimboHost
import dev.otherworld.nimbo.core.KeystoreSecretStore
import dev.otherworld.nimbo.ui.screens.OneOffRow
import dev.otherworld.nimbo.ui.screens.SectionHeader
import dev.otherworld.nimbo.ui.screens.TierCard
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.launch

fun createSupporterRepository(context: Context, scope: CoroutineScope): SupporterRepository =
    DirectKeySupporter(
        store = EncryptedKeyListStore(KeystoreSecretStore(context)),
        checker = KeyCheckClient(),
        cache = SupporterPrefs(context),
        scope = scope,
    )

private data class DirectOffer(val lookupKey: String, val tier: SupporterTier, val price: String)

// Display only: Stripe's checkout page always shows the real price before
// anyone pays. Change these if the Stripe prices change.
private val MONTHLY = listOf(
    DirectOffer("nimbo_supporter_monthly", SupporterTier.SUPPORTER, "£1.99 a month"),
    DirectOffer("nimbo_backer_monthly", SupporterTier.BACKER, "£4.99 a month"),
    DirectOffer("nimbo_patron_monthly", SupporterTier.PATRON, "£9.99 a month"),
)
private val ONE_OFF = listOf(
    DirectOffer("nimbo_tip_small", SupporterTier.ONE_OFF, "£2.99"),
    DirectOffer("nimbo_tip_medium", SupporterTier.ONE_OFF, "£5.99"),
    DirectOffer("nimbo_tip_large", SupporterTier.ONE_OFF, "£9.99"),
)

@Composable
fun SupportActions(repo: SupporterRepository) {
    val direct = repo as DirectKeySupporter
    val host = LocalNimboHost.current
    val keys by direct.keys.collectAsStateWithLifecycle()
    val scope = rememberCoroutineScope()
    var entry by rememberSaveable { mutableStateOf("") }
    var busy by remember { mutableStateOf(false) }

    fun manage() {
        scope.launch { direct.manageUrl()?.let(host::openUrl) }
    }

    SectionHeader("Monthly")
    MONTHLY.forEach { offer ->
        val action = monthlyActionFor(keys, offer.tier)
        TierCard(
            tier = offer.tier,
            price = offer.price,
            current = action == MonthlyAction.CURRENT,
            buttonLabel = when (action) {
                MonthlyAction.CHECKOUT -> "Choose ${offer.tier.label}"
                MonthlyAction.CURRENT -> "Your tier"
                MonthlyAction.PORTAL -> "Switch in Manage subscription"
            },
            enabled = action != MonthlyAction.CURRENT,
            onClick = {
                when (action) {
                    MonthlyAction.CHECKOUT -> host.openUrl(CHECKOUT_BASE + offer.lookupKey)
                    MonthlyAction.PORTAL -> manage()
                    MonthlyAction.CURRENT -> Unit
                }
            },
        )
    }

    SectionHeader("Say thanks once")
    Text(
        "A permanent supporter badge.",
        style = MaterialTheme.typography.bodySmall,
        color = MaterialTheme.colorScheme.onSurfaceVariant,
    )
    ONE_OFF.forEach { offer ->
        OneOffRow(price = offer.price, owned = false, enabled = true) {
            host.openUrl(CHECKOUT_BASE + offer.lookupKey)
        }
    }

    SectionHeader("Supporter key")
    Text(
        "After paying, tap \"Add to Nimbo\" on the checkout page, or paste the key from your email here.",
        style = MaterialTheme.typography.bodySmall,
        color = MaterialTheme.colorScheme.onSurfaceVariant,
    )
    OutlinedTextField(
        value = entry,
        onValueChange = { entry = it },
        label = { Text("Supporter key") },
        singleLine = true,
        modifier = Modifier.fillMaxWidth(),
    )
    Button(
        onClick = {
            scope.launch {
                busy = true
                try {
                    if (direct.addKey(entry)) entry = ""
                } finally {
                    busy = false
                }
            }
        },
        enabled = !busy && entry.isNotBlank(),
        shape = RoundedCornerShape(14.dp),
    ) {
        Text("Add key")
    }

    keys.forEach { key ->
        Row(modifier = Modifier.fillMaxWidth(), verticalAlignment = Alignment.CenterVertically) {
            Column(modifier = Modifier.weight(1f)) {
                Text(maskKey(key.key), style = MaterialTheme.typography.bodyMedium)
                Text(
                    when (key.state) {
                        KeyState.ACTIVE -> key.tier.label
                        KeyState.PAUSED -> "${key.tier.label}: payment problem"
                        KeyState.ENDED -> "Ended"
                        KeyState.UNCHECKED -> "Not checked yet"
                    },
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
            TextButton(onClick = { scope.launch { direct.removeKey(key.key) } }) { Text("Remove") }
        }
    }

    if (manageableKey(keys) != null) {
        OutlinedButton(onClick = ::manage, shape = RoundedCornerShape(14.dp)) {
            Text("Manage subscription")
        }
    }
}
