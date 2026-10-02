/*
 * KeyRules.kt — the pure rules for supporter keys: what a key looks like, what
 * the server's answers mean, and which answers are allowed to change anything.
 *
 * The one rule everything here serves: only a definite answer changes a tier.
 * Offline, a timeout, a 5xx, a captive-portal page — none of those is an
 * answer, and none of them takes a supporter's badge away.
 */
package dev.otherworld.nimbo.supporter

import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import java.net.URI
import java.net.URLDecoder

/** Otherworld Services keys: "owr_live_" + 20 characters of its alphabet. */
private val KEY_SHAPE = Regex("^owr_live_[abcdefghjkmnpqrstvwxyz23456789]{20}$")

const val RECHECK_AFTER_MS = 3L * 24 * 60 * 60 * 1000
const val CHECKOUT_BASE = "https://api.otherworld.dev/billing/checkout?price="

fun isKeyShaped(key: String): Boolean = KEY_SHAPE.matches(key.trim())

/** Enough to recognise a key, far too little to use. */
fun maskKey(key: String): String = key.take(13) + "…"

/** The key from a nimbo-supporter://add?key=… link, or null. */
fun keyFromLink(link: String): String? {
    val uri = runCatching { URI(link.trim()) }.getOrNull() ?: return null
    if (uri.scheme != "nimbo-supporter" || uri.host != "add") return null
    val key = uri.rawQuery
        ?.split('&')
        ?.map { it.split('=', limit = 2) }
        ?.firstOrNull { it.size == 2 && it[0] == "key" }
        ?.let { runCatching { URLDecoder.decode(it[1], "UTF-8") }.getOrNull() }
        ?: return null
    return key.trim().takeIf(::isKeyShaped)
}

fun tierFromServer(tier: String): SupporterTier = when (tier) {
    "nimbo_tip" -> SupporterTier.ONE_OFF
    "nimbo_supporter" -> SupporterTier.SUPPORTER
    "nimbo_backer" -> SupporterTier.BACKER
    "nimbo_patron" -> SupporterTier.PATRON
    // An active Nimbo key with a tier this version doesn't know (added later
    // on the server): still a supporter, so at least the badge.
    else -> SupporterTier.SUPPORTER
}

sealed interface CheckOutcome {
    data class Active(val tier: SupporterTier) : CheckOutcome
    /** The card failed and Stripe is retrying. Perks are kept meanwhile. */
    data object Paused : CheckOutcome
    /** Unknown, revoked, or not a Nimbo key. Perks go. */
    data object Ended : CheckOutcome
    /** No definite answer. Nothing changes. */
    data object Unreachable : CheckOutcome
}

private val json = Json { ignoreUnknownKeys = true }

private fun field(body: String?, name: String): String? = runCatching {
    json.parseToJsonElement(body ?: return null).jsonObject[name]?.jsonPrimitive?.content
}.getOrNull()

fun outcomeOf(code: Int, body: String?): CheckOutcome = when (code) {
    200 -> field(body, "tier")?.let { CheckOutcome.Active(tierFromServer(it)) } ?: CheckOutcome.Unreachable
    401 -> CheckOutcome.Ended
    403 -> when (field(body, "reason")) {
        "paused" -> CheckOutcome.Paused
        "revoked", "not_entitled" -> CheckOutcome.Ended
        else -> CheckOutcome.Unreachable
    }
    else -> CheckOutcome.Unreachable
}

enum class KeyState { UNCHECKED, ACTIVE, PAUSED, ENDED }

@Serializable
data class StoredKey(
    val key: String,
    val tier: SupporterTier = SupporterTier.NONE,
    val state: KeyState = KeyState.UNCHECKED,
)

fun StoredKey.after(outcome: CheckOutcome): StoredKey = when (outcome) {
    is CheckOutcome.Active -> copy(tier = outcome.tier, state = KeyState.ACTIVE)
    CheckOutcome.Paused -> copy(state = KeyState.PAUSED)
    CheckOutcome.Ended -> copy(state = KeyState.ENDED)
    CheckOutcome.Unreachable -> this
}

private fun StoredKey.counts() = state == KeyState.ACTIVE || state == KeyState.PAUSED

/** Paused keys still count: Stripe is retrying, and the badge stays meanwhile. */
fun tierOf(keys: List<StoredKey>): SupporterTier = highestTier(keys.filter { it.counts() }.map { it.tier })

fun dueForCheck(now: Long, lastCheckedAt: Long): Boolean = now - lastCheckedAt >= RECHECK_AFTER_MS

/**
 * The key to open the billing portal with: a live subscription, never a tip.
 * A paused key qualifies whatever its tier (a tip can't be paused, and a key
 * whose first check was "paused" has no tier yet but needs the portal to fix
 * its card).
 */
fun manageableKey(keys: List<StoredKey>): StoredKey? =
    keys.filter { it.counts() && (it.tier >= SupporterTier.SUPPORTER || it.state == KeyState.PAUSED) }
        .maxByOrNull { it.tier }

enum class MonthlyAction { CHECKOUT, CURRENT, PORTAL }

/**
 * What a monthly tier's button does. Someone already subscribed switches in
 * the billing portal; a second checkout would start a second subscription.
 */
fun monthlyActionFor(keys: List<StoredKey>, tier: SupporterTier): MonthlyAction {
    val current = manageableKey(keys) ?: return MonthlyAction.CHECKOUT
    return if (current.tier == tier) MonthlyAction.CURRENT else MonthlyAction.PORTAL
}
