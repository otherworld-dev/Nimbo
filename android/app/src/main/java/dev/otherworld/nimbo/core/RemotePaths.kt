/*
 * RemotePaths.kt — building the account-relative paths that rename, move and
 * create-folder send to the server.
 *
 * Pure, and tested, because the failure mode is silent: a rename that computes
 * the wrong target does not error, it relocates the file.
 */
package dev.otherworld.nimbo.core

private fun trimSlashes(path: String): String = path.trim().trim('/')

/** Joins [name] onto [parent]; the account root is "" and gains no leading slash. */
fun joinRemote(parent: String, name: String): String {
    val base = trimSlashes(parent)
    val leaf = trimSlashes(name)
    return when {
        leaf.isEmpty() -> base
        base.isEmpty() -> leaf
        else -> "$base/$leaf"
    }
}

/** The containing folder, or null for the account root (which has no parent). */
fun parentOfRemote(path: String): String? {
    val target = trimSlashes(path)
    if (target.isEmpty()) return null
    return if (target.contains('/')) target.substringBeforeLast('/') else ""
}

/**
 * Explains why [name] cannot be used, or null when it is fine.
 *
 * The messages are shown verbatim, so they are sentences. A leading dot is
 * allowed — Nextcloud accepts hidden names even though the local folder picker
 * does not.
 */
fun remoteNameProblem(name: String): String? {
    val trimmed = name.trim()
    return when {
        trimmed.isEmpty() -> "Enter a name."
        trimmed.contains('/') -> "Names can't contain “/”."
        trimmed.contains('\\') -> "Names can't contain “\\”."
        trimmed.all { it == '.' } -> "Choose a different name."
        else -> null
    }
}

/**
 * Where a rename should land: the same folder, new leaf. Null when [newName]
 * is not a usable name — a name carrying a separator would move the file
 * rather than rename it, which is never what "rename" meant.
 */
fun renameTargetRemote(path: String, newName: String): String? {
    if (remoteNameProblem(newName) != null) return null
    val parent = parentOfRemote(path) ?: return null
    return joinRemote(parent, newName.trim())
}
