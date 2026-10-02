/*
 * Cosmetics.kt — the extras supporters can choose, and what is actually shown.
 *
 * A choice is remembered even after its perk lapses, so resubscribing brings
 * it straight back. What is APPLIED is always worked out from the perks.
 */
package dev.otherworld.nimbo.supporter

/** One launcher alias per icon (see AndroidManifest.xml). */
enum class AppIcon(val aliasName: String, val label: String) {
    DEFAULT("dev.otherworld.nimbo.LauncherDefault", "Nimbo"),
    FOREST("dev.otherworld.nimbo.LauncherForest", "Forest"),
    EMBER("dev.otherworld.nimbo.LauncherEmber", "Ember"),
    SLATE("dev.otherworld.nimbo.LauncherSlate", "Slate");

    companion object {
        fun fromStored(value: String?): AppIcon = entries.firstOrNull { it.name == value } ?: DEFAULT
    }
}

/**
 * A fixed set, not a free colour picker: Theming.kt guarantees an app you can
 * read whatever the server sends, and fixed colours keep that guarantee (the
 * unit tests hold each one to it, on both themes).
 */
enum class AccentChoice(val argb: Long?, val label: String) {
    FOLLOW_NEXTCLOUD(null, "Follow Nextcloud"),
    INDIGO(0xFF5856E0, "Indigo"),
    TEAL(0xFF00796B, "Teal"),
    FOREST(0xFF2E7D32, "Forest"),
    CRIMSON(0xFFC62828, "Crimson"),
    OCEAN(0xFF1871D8, "Ocean"),
    VIOLET(0xFFAA42D7, "Violet");

    companion object {
        fun fromStored(value: String?): AccentChoice =
            entries.firstOrNull { it.name == value } ?: FOLLOW_NEXTCLOUD
    }
}

fun effectiveIcon(chosen: AppIcon, perks: Perks): AppIcon = if (perks.icons) chosen else AppIcon.DEFAULT

fun effectiveAccent(chosen: AccentChoice, perks: Perks): AccentChoice =
    if (perks.accents) chosen else AccentChoice.FOLLOW_NEXTCLOUD
