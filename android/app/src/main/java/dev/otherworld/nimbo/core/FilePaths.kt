/*
 * FilePaths.kt — mapping remote (server) paths onto the local copies the sync
 * engine keeps, and the sync state that follows from it.
 *
 * Pure string arithmetic, deliberately free of Android and of the engine, so it
 * can be tested exhaustively on the JVM. The file browser's whole differentiator
 * — knowing what is actually on this phone — rests on getting this right.
 */
package dev.otherworld.nimbo.core

/** What the file browser can tell the user about a server file. */
enum class SyncState {
    /** Inside a synced folder and present on this device. */
    SYNCED,

    /** Not inside any synced folder — opening it means downloading it. */
    SERVER_ONLY,

    /** Inside a synced folder but not on disk yet (queued, or mid-transfer). */
    PENDING,

    /** The engine has flagged this path as conflicted. */
    CONFLICTED,
}

/** Trims separators so both sides of a comparison have the same shape. */
private fun norm(path: String): String = path.trim().trim('/')

/**
 * The absolute local path a remote path syncs to, or null when no configured
 * pair covers it.
 *
 * Matching is per path segment, never a raw string prefix: "Photos2" is not
 * inside "Photos". When pairs are nested the deepest root wins, so a camera-roll
 * pair inside a broader Photos pair still resolves to the camera roll.
 */
fun localPathFor(remotePath: String, pairs: List<SyncPair>): String? {
    val target = norm(remotePath)
    var bestLocal: String? = null
    var bestRootLength = -1

    for (pair in pairs) {
        val root = norm(pair.remoteRoot)
        val localRoot = pair.localDir.trim().trimEnd('/')
        if (localRoot.isEmpty()) continue

        val relative: String = when {
            // A pair rooted at the account root covers every path.
            root.isEmpty() -> target
            target == root -> ""
            target.startsWith("$root/") -> target.removePrefix("$root/")
            else -> continue
        }
        // Longer root = more specific pair; ties cannot occur (roots are distinct).
        if (root.length > bestRootLength) {
            bestRootLength = root.length
            bestLocal = if (relative.isEmpty()) localRoot else "$localRoot/$relative"
        }
    }
    return bestLocal
}

/**
 * Classifies a remote path for display.
 *
 * Precedence is deliberate: a conflict outranks everything (it is the state the
 * user has to act on), then an in-flight transfer, then whether the file is
 * actually on disk. [existsLocally] is injected rather than probed here so the
 * decision stays pure; the repository supplies the real filesystem check.
 */
fun syncStateFor(
    remotePath: String,
    pairs: List<SyncPair>,
    existsLocally: (String) -> Boolean,
    conflictedPaths: Set<String> = emptySet(),
    inFlightPaths: Set<String> = emptySet(),
): SyncState {
    val target = norm(remotePath)
    if (conflictedPaths.any { norm(it) == target }) return SyncState.CONFLICTED
    val local = localPathFor(target, pairs) ?: return SyncState.SERVER_ONLY
    if (inFlightPaths.any { norm(it) == target }) return SyncState.PENDING
    return if (existsLocally(local)) SyncState.SYNCED else SyncState.PENDING
}

