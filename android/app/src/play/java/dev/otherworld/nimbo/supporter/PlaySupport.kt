/*
 * PlaySupport.kt — the Play build's half of the supporter seam. Nothing here
 * links outside Google Play: Play's payment policy forbids it.
 */
package dev.otherworld.nimbo.supporter

import android.app.Activity
import android.content.Context
import android.content.ContextWrapper
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.LocalUriHandler
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import dev.otherworld.nimbo.ui.screens.OneOffRow
import dev.otherworld.nimbo.ui.screens.SectionHeader
import dev.otherworld.nimbo.ui.screens.TierCard
import kotlinx.coroutines.CoroutineScope

fun createSupporterRepository(context: Context, scope: CoroutineScope): SupporterRepository =
    PlayBillingSupporter(context, SupporterPrefs(context), scope)

private tailrec fun Context.findActivity(): Activity? = when (this) {
    is Activity -> this
    is ContextWrapper -> baseContext.findActivity()
    else -> null
}

@Composable
fun SupportActions(repo: SupporterRepository) {
    val play = repo as PlayBillingSupporter
    val offers by play.offers.collectAsStateWithLifecycle()
    val owned by play.owned.collectAsStateWithLifecycle()
    val activity = LocalContext.current.findActivity()
    // ACTION_VIEW rather than a Custom Tab, so the Play Store app opens it.
    val uri = LocalUriHandler.current

    if (offers.isEmpty()) {
        Text(
            "Loading prices from Google Play…",
            style = MaterialTheme.typography.bodyMedium,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
        return
    }

    val current = currentSubscription(owned)

    SectionHeader("Monthly")
    offers.filter { it.productId in PlayProducts.subscriptions }.forEach { offer ->
        val change = changeFor(current, offer.productId)
        TierCard(
            tier = offer.tier,
            price = offer.price,
            current = change == Change.SAME,
            buttonLabel = when (change) {
                Change.NEW -> "Choose ${offer.tier.label}"
                Change.SAME -> "Your tier"
                Change.UPGRADE -> "Upgrade now"
                Change.DOWNGRADE -> "Switch at renewal"
            },
            enabled = change != Change.SAME && activity != null,
            onClick = { activity?.let { play.buy(it, offer.productId) } },
        )
    }

    SectionHeader("Say thanks once")
    Text(
        "A permanent supporter badge.",
        style = MaterialTheme.typography.bodySmall,
        color = MaterialTheme.colorScheme.onSurfaceVariant,
    )
    val tips = offers.filter { it.productId in PlayProducts.tips }
    tips.forEach { offer ->
        val bought = owned.any { it.productId == offer.productId && it.purchased }
        OneOffRow(price = offer.price, owned = bought, enabled = !bought && activity != null) {
            activity?.let { play.buy(it, offer.productId) }
        }
    }

    if (anyPending(owned)) {
        Text(
            "Payment pending: your support unlocks when Google Play confirms it.",
            style = MaterialTheme.typography.bodySmall,
        )
    }

    if (current != null) {
        OutlinedButton(
            onClick = { uri.openUri(manageUrl(current.productId)) },
            shape = RoundedCornerShape(14.dp),
        ) {
            Text("Manage or cancel")
        }
    }
}
