/*
 * DirectKeySupporter.kt — supporter status for builds outside Google Play:
 * keys bought through Otherworld's checkout, checked against Otherworld
 * Services. Without a key it never contacts anyone.
 */
package dev.otherworld.nimbo.supporter

import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock

class DirectKeySupporter(
    private val store: KeyListStore,
    private val checker: KeyChecker,
    private val cache: StatusCache,
    private val scope: CoroutineScope,
    private val clock: () -> Long = System::currentTimeMillis,
) : SupporterRepository {

    private val _keys = MutableStateFlow(store.load())
    val keys: StateFlow<List<StoredKey>> = _keys.asStateFlow()

    private val _status = MutableStateFlow(cache.read())
    override val status: StateFlow<SupporterStatus> = _status.asStateFlow()

    private val _notice = MutableStateFlow<SupporterNotice?>(null)
    override val notice: StateFlow<SupporterNotice?> = _notice.asStateFlow()

    /** One check or edit at a time, so two refreshes can't interleave saves. */
    private val mutex = Mutex()

    override suspend fun refresh(force: Boolean) {
        mutex.withLock {
            val current = _keys.value
            if (current.isEmpty()) {
                publish(current, definite = true)
                return
            }
            val due = dueForCheck(clock(), _status.value.checkedAt) ||
                current.any { it.state == KeyState.UNCHECKED }
            if (!force && !due) return

            val outcomes = current.map { it to checker.check(it.key) }
            val updated = outcomes.map { (key, outcome) -> key.after(outcome) }
            val definite = outcomes.any { it.second != CheckOutcome.Unreachable }
            save(updated)
            publish(updated, definite)

            when {
                outcomes.any { it.second == CheckOutcome.Paused } -> _notice.value =
                    SupporterNotice("Payment problem: update it in Manage subscription.", isError = true)
                force && !definite -> _notice.value =
                    SupporterNotice("Couldn't check right now. Your status hasn't changed.", isError = true)
            }
        }
    }

    /** Adds a key. True if it was kept (active, paused, or saved to check later). */
    suspend fun addKey(raw: String): Boolean = mutex.withLock {
        val key = raw.trim()
        if (!isKeyShaped(key)) {
            _notice.value = SupporterNotice(
                "That doesn't look like a supporter key. It starts with owr_live_.",
                isError = true,
            )
            return false
        }
        if (_keys.value.any { it.key == key }) {
            _notice.value = SupporterNotice("That key is already added.")
            return false
        }
        val outcome = checker.check(key)
        if (outcome == CheckOutcome.Ended) {
            _notice.value = SupporterNotice("That key isn't recognised, or has ended.", isError = true)
            return false
        }
        val updated = _keys.value + StoredKey(key).after(outcome)
        save(updated)
        publish(updated, definite = outcome != CheckOutcome.Unreachable)
        _notice.value = when (outcome) {
            is CheckOutcome.Active -> SupporterNotice(
                if (outcome.tier == SupporterTier.ONE_OFF) "Key added. Thank you for supporting Nimbo."
                else "Key added. You're a ${outcome.tier.label}. Thank you.",
            )
            CheckOutcome.Paused -> SupporterNotice(
                "Key added, but its payment has a problem: update it in Manage subscription.",
                isError = true,
            )
            else -> SupporterNotice("Key saved. It couldn't be checked right now and will be checked again later.")
        }
        true
    }

    suspend fun removeKey(key: String) {
        mutex.withLock {
            val updated = _keys.value.filterNot { it.key == key }
            save(updated)
            publish(updated, definite = true)
        }
    }

    /** The billing portal for the subscription key, or null with a notice. */
    suspend fun manageUrl(): String? {
        val key = manageableKey(_keys.value) ?: run {
            _notice.value = SupporterNotice("There's no subscription to manage.")
            return null
        }
        return checker.portalUrl(key.key) ?: run {
            _notice.value = SupporterNotice("Couldn't open subscription management. Try again.", isError = true)
            null
        }
    }

    override fun handleLink(link: String): Boolean {
        val key = keyFromLink(link) ?: return false
        scope.launch { addKey(key) }
        return true
    }

    override fun clearNotice() {
        _notice.value = null
    }

    private fun save(keys: List<StoredKey>) {
        _keys.value = keys
        store.save(keys)
    }

    private fun publish(keys: List<StoredKey>, definite: Boolean) {
        val tier = tierOf(keys)
        val next = SupporterStatus(
            tier = tier,
            source = if (keys.isEmpty()) SupporterSource.NONE else SupporterSource.KEY,
            checkedAt = if (definite) clock() else _status.value.checkedAt,
        )
        _status.value = next
        cache.write(next)
    }
}
