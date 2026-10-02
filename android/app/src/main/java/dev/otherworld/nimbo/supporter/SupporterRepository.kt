/*
 * SupporterRepository.kt — the one seam between the shared supporter UI and
 * how a build takes payment. `play` implements it with Google Play Billing,
 * `direct` with Otherworld Services supporter keys; each flavour also supplies
 * `createSupporterRepository(context, scope)` and the `SupportActions()`
 * composable in this same package.
 */
package dev.otherworld.nimbo.supporter

import kotlinx.coroutines.flow.StateFlow

interface SupporterRepository {
    val status: StateFlow<SupporterStatus>
    val notice: StateFlow<SupporterNotice?>

    /**
     * Re-reads the status. [force] = false lets an implementation skip a check
     * it made recently (app start); true is a user looking at the Support
     * screen. Never throws, and only a definite answer changes the tier.
     */
    suspend fun refresh(force: Boolean)

    /** A link the app was opened with. True if this build handled it. */
    fun handleLink(link: String): Boolean

    fun clearNotice()
}

/** Where the last status is kept between launches. */
interface StatusCache {
    fun read(): SupporterStatus
    fun write(status: SupporterStatus)
}
