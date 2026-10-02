/*
 * SupporterViewModel.kt — the supporter state the screens render: the
 * repository's status and notice, combined with this phone's choices.
 */
package dev.otherworld.nimbo.supporter

import android.app.Application
import androidx.lifecycle.AndroidViewModel
import androidx.lifecycle.viewModelScope
import dev.otherworld.nimbo.BuildConfig
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.stateIn
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

class SupporterViewModel(app: Application) : AndroidViewModel(app) {

    private val prefs = SupporterPrefs(app)
    private val repo = Supporter.repository
    private val choices = MutableStateFlow(prefs.choices())

    val ui: StateFlow<SupporterUi> = combine(repo.status, repo.notice, choices) { status, notice, c ->
        supporterUi(status, notice, c)
    }.stateIn(
        viewModelScope,
        SharingStarted.Eagerly,
        supporterUi(repo.status.value, repo.notice.value, choices.value),
    )

    init {
        // Supporting from anywhere retires the Home card for good. The REAL
        // tier, so the debug override can't retire it while testing the card.
        viewModelScope.launch {
            ui.map { it.status.tier }.distinctUntilChanged().collect { tier ->
                if (tier != SupporterTier.NONE && !choices.value.nudgeRetired) retireNudge()
            }
        }
        // The icon follows the perks: a lapsed Backer gets the default back,
        // a returning one gets their choice back.
        viewModelScope.launch {
            ui.map { it.effectiveIcon }.distinctUntilChanged().collect { icon ->
                withContext(Dispatchers.IO) { AppIconSwitcher.apply(getApplication(), icon) }
            }
        }
    }

    /** The user is looking at the Support screen: always ask. */
    fun refresh() {
        viewModelScope.launch { repo.refresh(force = true) }
    }

    fun clearNotice() = repo.clearNotice()

    fun handleLink(link: String): Boolean = repo.handleLink(link)

    fun retireNudge() {
        prefs.nudgeRetired = true
        choices.value = choices.value.copy(nudgeRetired = true)
    }

    fun noteHomeSeen(now: Long = System.currentTimeMillis()) {
        if (choices.value.firstSeenAt != 0L) return
        prefs.firstSeenAt = now
        choices.value = choices.value.copy(firstSeenAt = now)
    }

    fun setDebugOverride(tier: SupporterTier?) {
        if (!BuildConfig.DEBUG) return
        prefs.debugOverride = tier
        choices.value = choices.value.copy(debugOverride = tier)
    }

    fun setIcon(icon: AppIcon) {
        prefs.icon = icon
        choices.value = choices.value.copy(icon = icon)
    }

    fun setAccent(accent: AccentChoice) {
        prefs.accent = accent
        choices.value = choices.value.copy(accent = accent)
    }
}
