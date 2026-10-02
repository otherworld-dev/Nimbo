/*
 * PlayRules.kt — the pure rules for Play purchases. PlayBillingSupporter
 * turns Play's objects into OwnedProduct and asks these what they mean.
 */
package dev.otherworld.nimbo.supporter

object PlayProducts {
    const val SUPPORTER = "supporter_monthly"
    const val BACKER = "backer_monthly"
    const val PATRON = "patron_monthly"
    const val TIP_SMALL = "tip_small"
    const val TIP_MEDIUM = "tip_medium"
    const val TIP_LARGE = "tip_large"

    val subscriptions = listOf(SUPPORTER, BACKER, PATRON)
    /** Non-consumable, so Play returns them on every device, after every reinstall. */
    val tips = listOf(TIP_SMALL, TIP_MEDIUM, TIP_LARGE)
}

fun tierForProduct(productId: String): SupporterTier = when (productId) {
    PlayProducts.SUPPORTER -> SupporterTier.SUPPORTER
    PlayProducts.BACKER -> SupporterTier.BACKER
    PlayProducts.PATRON -> SupporterTier.PATRON
    in PlayProducts.tips -> SupporterTier.ONE_OFF
    else -> SupporterTier.NONE
}

/** One product in one Play purchase, reduced to what the rules need. */
data class OwnedProduct(
    val productId: String,
    val token: String,
    val purchased: Boolean,
    val pending: Boolean,
    val acknowledged: Boolean,
)

fun playTier(owned: List<OwnedProduct>): SupporterTier =
    highestTier(owned.filter { it.purchased }.map { tierForProduct(it.productId) })

/** Play refunds anything not acknowledged within 3 days, so every refresh does it. */
fun needsAcknowledging(owned: List<OwnedProduct>): List<OwnedProduct> =
    owned.filter { it.purchased && !it.acknowledged }

fun anyPending(owned: List<OwnedProduct>): Boolean = owned.any { it.pending }

fun currentSubscription(owned: List<OwnedProduct>): OwnedProduct? =
    owned.filter { it.purchased && it.productId in PlayProducts.subscriptions }
        .maxByOrNull { tierForProduct(it.productId) }

enum class Change { NEW, SAME, UPGRADE, DOWNGRADE }

fun changeFor(current: OwnedProduct?, target: String): Change {
    if (current == null) return Change.NEW
    val from = tierForProduct(current.productId)
    val to = tierForProduct(target)
    return when {
        from == to -> Change.SAME
        to > from -> Change.UPGRADE
        else -> Change.DOWNGRADE
    }
}

fun manageUrl(productId: String?): String =
    "https://play.google.com/store/account/subscriptions?package=dev.otherworld.nimbo" +
        (productId?.let { "&sku=$it" } ?: "")

fun unavailableMessage(billingUnavailable: Boolean): String =
    if (billingUnavailable) "Google Play purchases aren't available on this device."
    else "Couldn't reach Google Play. Try again."
