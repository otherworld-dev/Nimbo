/*
 * SupporterModel.kt — what a supporter is, kept pure.
 *
 * Nimbo is free with every feature. Supporting it buys thanks and a few
 * cosmetic extras, and every rule about who gets which lives here, so the
 * screens never decide anything and the rules are testable without Android.
 */
package dev.otherworld.nimbo.supporter

/** Ordered: a later tier always includes everything an earlier one gets. */
enum class SupporterTier(val label: String) {
    NONE(""),
    /** Paid once: a permanent badge, no cosmetics. */
    ONE_OFF("Supporter"),
    SUPPORTER("Supporter"),
    BACKER("Backer"),
    PATRON("Patron"),
}

data class Perks(val badge: Boolean, val icons: Boolean, val accents: Boolean) {
    companion object {
        fun of(tier: SupporterTier) = Perks(
            badge = tier != SupporterTier.NONE,
            icons = tier >= SupporterTier.BACKER,
            accents = tier >= SupporterTier.PATRON,
        )
    }
}

/** Someone with a tip and a membership is whichever is higher. */
fun highestTier(tiers: Iterable<SupporterTier>): SupporterTier = tiers.maxOrNull() ?: SupporterTier.NONE

/** What each tier gets, for the tier cards. */
fun perkLines(tier: SupporterTier): List<String> = when (tier) {
    SupporterTier.NONE -> emptyList()
    SupporterTier.ONE_OFF -> listOf("Permanent supporter badge")
    SupporterTier.SUPPORTER -> listOf("Supporter badge", "Our thanks, every month")
    SupporterTier.BACKER -> listOf("Everything in Supporter", "Alternative app icons")
    SupporterTier.PATRON -> listOf("Everything in Backer", "Accent colour themes")
}

enum class SupporterSource { NONE, PLAY, KEY }

/** The last known answer, cached so the first frame knows the tier offline. */
data class SupporterStatus(
    val tier: SupporterTier = SupporterTier.NONE,
    val source: SupporterSource = SupporterSource.NONE,
    /** When the source last gave a definite answer (ms); 0 = never. */
    val checkedAt: Long = 0L,
) {
    fun encode(): String = "${tier.name}|${source.name}|$checkedAt"

    companion object {
        /** Anything unreadable is "not a supporter yet", never a crash. */
        fun decode(raw: String?): SupporterStatus {
            val parts = raw?.split('|') ?: return SupporterStatus()
            if (parts.size != 3) return SupporterStatus()
            val tier = SupporterTier.entries.firstOrNull { it.name == parts[0] } ?: return SupporterStatus()
            val source = SupporterSource.entries.firstOrNull { it.name == parts[1] } ?: return SupporterStatus()
            val at = parts[2].toLongOrNull() ?: return SupporterStatus()
            return SupporterStatus(tier, source, at)
        }
    }
}

/** One line for the Support screen. Never a toast. */
data class SupporterNotice(val text: String, val isError: Boolean = false)

/** What the user chose on this phone, independent of whether it's unlocked. */
data class SupporterChoices(
    val icon: AppIcon = AppIcon.DEFAULT,
    val accent: AccentChoice = AccentChoice.FOLLOW_NEXTCLOUD,
    /** First time the Sync tab's Home was shown with an account (ms); 0 = not yet. */
    val firstSeenAt: Long = 0L,
    val nudgeRetired: Boolean = false,
)

/** Everything the supporter UI renders from. */
data class SupporterUi(
    /** The real answer from Play or the key check. */
    val status: SupporterStatus = SupporterStatus(),
    /** The tier the UI acts on. */
    val tier: SupporterTier = SupporterTier.NONE,
    val perks: Perks = Perks.of(SupporterTier.NONE),
    val notice: SupporterNotice? = null,
    val icon: AppIcon = AppIcon.DEFAULT,
    val accent: AccentChoice = AccentChoice.FOLLOW_NEXTCLOUD,
    val effectiveIcon: AppIcon = AppIcon.DEFAULT,
    val effectiveAccent: AccentChoice = AccentChoice.FOLLOW_NEXTCLOUD,
    val firstSeenAt: Long = 0L,
    val nudgeRetired: Boolean = false,
)

fun supporterUi(status: SupporterStatus, notice: SupporterNotice?, choices: SupporterChoices): SupporterUi {
    val tier = status.tier
    val perks = Perks.of(tier)
    return SupporterUi(
        status = status,
        tier = tier,
        perks = perks,
        notice = notice,
        icon = choices.icon,
        accent = choices.accent,
        effectiveIcon = effectiveIcon(choices.icon, perks),
        effectiveAccent = effectiveAccent(choices.accent, perks),
        firstSeenAt = choices.firstSeenAt,
        nudgeRetired = choices.nudgeRetired,
    )
}
