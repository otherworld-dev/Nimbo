/*
 * PlayBillingSupporter.kt — supporter status from Google Play.
 *
 * Play keeps its own copy of purchases on the device, so this works offline.
 * Purchases are trusted as Play reports them, with no server check: someone
 * faking one on a rooted phone gets cosmetics, nothing else.
 */
package dev.otherworld.nimbo.supporter

import android.app.Activity
import android.content.Context
import android.util.Log
import com.android.billingclient.api.AcknowledgePurchaseParams
import com.android.billingclient.api.BillingClient
import com.android.billingclient.api.BillingClient.BillingResponseCode
import com.android.billingclient.api.BillingClient.ProductType
import com.android.billingclient.api.BillingClientStateListener
import com.android.billingclient.api.BillingFlowParams
import com.android.billingclient.api.BillingFlowParams.SubscriptionUpdateParams.ReplacementMode
import com.android.billingclient.api.BillingResult
import com.android.billingclient.api.PendingPurchasesParams
import com.android.billingclient.api.ProductDetails
import com.android.billingclient.api.Purchase
import com.android.billingclient.api.PurchasesUpdatedListener
import com.android.billingclient.api.QueryProductDetailsParams
import com.android.billingclient.api.QueryPurchasesParams
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch
import kotlinx.coroutines.suspendCancellableCoroutine
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlin.coroutines.resume

data class PlayOffer(val productId: String, val tier: SupporterTier, val price: String)

class PlayBillingSupporter(
    context: Context,
    private val cache: StatusCache,
    private val scope: CoroutineScope,
    private val clock: () -> Long = System::currentTimeMillis,
) : SupporterRepository, PurchasesUpdatedListener {

    private val client: BillingClient = BillingClient.newBuilder(context.applicationContext)
        .setListener(this)
        .enablePendingPurchases(PendingPurchasesParams.newBuilder().enableOneTimeProducts().build())
        .enableAutoServiceReconnection()
        .build()

    private val _status = MutableStateFlow(cache.read())
    override val status: StateFlow<SupporterStatus> = _status.asStateFlow()

    private val _notice = MutableStateFlow<SupporterNotice?>(null)
    override val notice: StateFlow<SupporterNotice?> = _notice.asStateFlow()

    private val _offers = MutableStateFlow<List<PlayOffer>>(emptyList())
    val offers: StateFlow<List<PlayOffer>> = _offers.asStateFlow()

    private val _owned = MutableStateFlow<List<OwnedProduct>>(emptyList())
    val owned: StateFlow<List<OwnedProduct>> = _owned.asStateFlow()

    private var details: Map<String, ProductDetails> = emptyMap()
    private val mutex = Mutex()

    override suspend fun refresh(force: Boolean) {
        mutex.withLock {
            val setup = connect()
            if (setup != BillingResponseCode.OK) {
                if (force) {
                    _notice.value = SupporterNotice(
                        unavailableMessage(setup == BillingResponseCode.BILLING_UNAVAILABLE),
                        isError = true,
                    )
                }
                return
            }
            // Either query failing is no answer: keep what we had.
            val subs = queryPurchases(ProductType.SUBS) ?: return
            val tips = queryPurchases(ProductType.INAPP) ?: return
            val owned = (subs + tips).flatMap { purchase ->
                purchase.products.map { id ->
                    OwnedProduct(
                        productId = id,
                        token = purchase.purchaseToken,
                        purchased = purchase.purchaseState == Purchase.PurchaseState.PURCHASED,
                        pending = purchase.purchaseState == Purchase.PurchaseState.PENDING,
                        acknowledged = purchase.isAcknowledged,
                    )
                }
            }
            needsAcknowledging(owned).map { it.token }.distinct().forEach { acknowledge(it) }
            _owned.value = owned

            val next = SupporterStatus(playTier(owned), SupporterSource.PLAY, clock())
            _status.value = next
            cache.write(next)

            if (details.isEmpty()) loadOffers()
        }
    }

    /** Starts Play's purchase sheet. Upgrades now, downgrades at renewal. */
    fun buy(activity: Activity, productId: String) {
        val product = details[productId] ?: run {
            _notice.value = SupporterNotice(unavailableMessage(billingUnavailable = false), isError = true)
            return
        }
        val productParams = BillingFlowParams.ProductDetailsParams.newBuilder()
            .setProductDetails(product)
            .apply { product.subscriptionOfferDetails?.firstOrNull()?.offerToken?.let(::setOfferToken) }
            .build()
        val flow = BillingFlowParams.newBuilder().setProductDetailsParamsList(listOf(productParams))

        val current = currentSubscription(_owned.value)
        if (product.productType == ProductType.SUBS && current != null) {
            when (changeFor(current, productId)) {
                Change.SAME -> return
                Change.UPGRADE, Change.DOWNGRADE -> flow.setSubscriptionUpdateParams(
                    BillingFlowParams.SubscriptionUpdateParams.newBuilder()
                        .setOldPurchaseToken(current.token)
                        .setSubscriptionReplacementMode(
                            if (changeFor(current, productId) == Change.UPGRADE) ReplacementMode.WITH_TIME_PRORATION
                            else ReplacementMode.DEFERRED,
                        )
                        .build(),
                )
                Change.NEW -> Unit
            }
        }

        val result = client.launchBillingFlow(activity, flow.build())
        if (result.responseCode != BillingResponseCode.OK) onPurchasesUpdated(result, null)
    }

    override fun onPurchasesUpdated(result: BillingResult, purchases: MutableList<Purchase>?) {
        when (result.responseCode) {
            BillingResponseCode.OK, BillingResponseCode.ITEM_ALREADY_OWNED ->
                scope.launch { refresh(force = true) }
            BillingResponseCode.USER_CANCELED -> Unit
            else -> {
                Log.i(TAG, "purchase did not complete: ${result.responseCode}")
                _notice.value = SupporterNotice("Couldn't complete the purchase. Try again.", isError = true)
            }
        }
    }

    override fun handleLink(link: String): Boolean = false

    override fun clearNotice() {
        _notice.value = null
    }

    private suspend fun connect(): Int {
        if (client.isReady) return BillingResponseCode.OK
        val code = suspendCancellableCoroutine<Int> { cont ->
            client.startConnection(object : BillingClientStateListener {
                override fun onBillingSetupFinished(result: BillingResult) {
                    if (cont.isActive) cont.resume(result.responseCode)
                }

                override fun onBillingServiceDisconnected() {
                    // Automatic service reconnection handles this.
                }
            })
        }
        if (code != BillingResponseCode.OK) Log.i(TAG, "billing setup: $code")
        return code
    }

    private suspend fun queryPurchases(type: String): List<Purchase>? = suspendCancellableCoroutine { cont ->
        client.queryPurchasesAsync(QueryPurchasesParams.newBuilder().setProductType(type).build()) { result, list ->
            if (cont.isActive) cont.resume(if (result.responseCode == BillingResponseCode.OK) list else null)
        }
    }

    private suspend fun acknowledge(token: String) = suspendCancellableCoroutine<Unit> { cont ->
        client.acknowledgePurchase(AcknowledgePurchaseParams.newBuilder().setPurchaseToken(token).build()) { result ->
            if (result.responseCode != BillingResponseCode.OK) Log.i(TAG, "acknowledge failed: ${result.responseCode}")
            if (cont.isActive) cont.resume(Unit)
        }
    }

    private suspend fun loadOffers() {
        val subs = queryDetails(ProductType.SUBS, PlayProducts.subscriptions)
        val tips = queryDetails(ProductType.INAPP, PlayProducts.tips)
        details = (subs + tips).associateBy { it.productId }
        _offers.value = (PlayProducts.subscriptions + PlayProducts.tips).mapNotNull { id ->
            val product = details[id] ?: return@mapNotNull null
            val price = product.subscriptionOfferDetails?.firstOrNull()
                ?.pricingPhases?.pricingPhaseList?.lastOrNull()?.formattedPrice
                ?.let { "$it a month" }
                ?: product.oneTimePurchaseOfferDetails?.formattedPrice
                ?: return@mapNotNull null
            PlayOffer(id, tierForProduct(id), price)
        }
    }

    /** One product type per query: Play doesn't mix them. */
    private suspend fun queryDetails(type: String, ids: List<String>): List<ProductDetails> =
        suspendCancellableCoroutine { cont ->
            val params = QueryProductDetailsParams.newBuilder()
                .setProductList(ids.map { QueryProductDetailsParams.Product.newBuilder().setProductId(it).setProductType(type).build() })
                .build()
            client.queryProductDetailsAsync(params) { result, found ->
                val list = if (result.responseCode == BillingResponseCode.OK) found.productDetailsList else emptyList()
                if (cont.isActive) cont.resume(list)
            }
        }

    private companion object {
        const val TAG = "NimboSupporter"
    }
}
