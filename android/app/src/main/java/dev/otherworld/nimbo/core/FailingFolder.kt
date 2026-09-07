/*
 * FailingFolder.kt — per-folder sync health, and the one rule the UI owes the
 * user because of it.
 *
 * The engine reports ONE status for the whole account. When folders disagree,
 * whichever finished last wins, so a healthy folder's "Up to date" can be on
 * screen while another folder has stopped syncing completely. That is the worst
 * thing a sync client can do: it is not a missing feature, it is the app being
 * wrong about the only question it exists to answer.
 *
 * These helpers are pure so the rule is tested rather than assumed.
 */
package dev.otherworld.nimbo.core

import kotlinx.serialization.Serializable

/** A sync folder whose last pass failed. Healthy folders are simply absent. */
@Serializable
data class FailingFolder(
    val localDir: String = "",
    /** The engine's own message — shown to the user, not translated away. */
    val lastError: String = "",
    /** RFC 3339, when it STARTED failing; "" if the engine did not say. */
    val since: String = "",
) {
    /** Leaf of the local path: what the user calls this folder. */
    val name: String
        get() = localDir.trimEnd('/').substringAfterLast('/').ifBlank { localDir }
}

/**
 * Whether the engine's [status] can be taken at face value.
 *
 * False whenever any folder is failing, regardless of what the status says —
 * the status is account-wide and cannot see the disagreement.
 */
fun isEverythingHealthy(status: String, failing: List<FailingFolder>): Boolean =
    failing.isEmpty()

/**
 * A short line naming what is wrong, or null when nothing is. Names the folder
 * when there is exactly one, because "Photos isn't syncing" is actionable in a
 * way that "1 folder isn't syncing" is not.
 */
fun failingSummary(failing: List<FailingFolder>): String? = when {
    failing.isEmpty() -> null
    failing.size == 1 -> "${failing.first().name} isn't syncing"
    else -> "${failing.size} folders aren't syncing"
}
