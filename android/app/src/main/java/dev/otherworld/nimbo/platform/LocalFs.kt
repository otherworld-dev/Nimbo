/*
 * LocalFs.kt — the tiny slice of the local filesystem the "where should this
 * live on my phone?" picker needs: list folders, walk up, make a folder, and
 * turn a POSIX path into something a human recognises.
 *
 * Deliberately plain java.io: the app holds MANAGE_EXTERNAL_STORAGE, and the Go
 * engine syncs to real POSIX paths — the Storage Access Framework hands back
 * content:// URIs the engine cannot open, so it is not used anywhere here.
 *
 * Nothing in this file throws. listFiles() returns null on a permission
 * failure (and on plenty of OEM quirks besides), so every read is guarded and
 * degrades to "empty" rather than taking the UI down with it.
 */
package dev.otherworld.nimbo.platform

import android.os.Environment
import java.io.File

/** One directory in the browser: its path, its display name, and how many visible folders it holds. */
data class LocalDir(
    val path: String,
    val name: String,
    val childDirCount: Int,
    /** Visible files directly inside, so a folder of files is not called "Empty". */
    val childFileCount: Int = 0,
)

/**
 * What a folder row says about its contents.
 *
 * Counting only subfolders described a folder holding 60 files as "Empty" —
 * seen on device at exactly the moment the user is choosing where files should
 * live. A negative count means "could not read", which must not read as empty.
 */
fun folderSubtitle(dirs: Int, files: Int): String {
    if (dirs < 0 || files < 0) return ""
    val parts = buildList {
        if (dirs > 0) add(plural(dirs, "folder"))
        if (files > 0) add(plural(files, "file"))
    }
    return if (parts.isEmpty()) "Empty" else parts.joinToString(" · ")
}

private fun plural(n: Int, noun: String): String = if (n == 1) "$n $noun" else "$n ${noun}s"


object LocalFs {

    /** Label shown in place of the shared-storage root, which users never call by its path. */
    private const val ROOT_LABEL = "Internal storage"

    /** Root of the shared storage volume — /storage/emulated/0 on a normal device. */
    @Suppress("DEPRECATION")
    fun externalRoot(): String = runCatching {
        Environment.getExternalStorageDirectory().absolutePath
    }.getOrDefault("/storage/emulated/0")

    /**
     * Visible sub-directories of [path], sorted the way a file manager would:
     * folders only, no dot-directories, case-insensitive by name.
     */
    fun listDirs(path: String): List<LocalDir> = runCatching {
        val children = File(normalize(path)).listFiles() ?: return emptyList()
        children
            .filter { it.isDirectory && !it.name.startsWith(".") }
            .sortedWith(compareBy(String.CASE_INSENSITIVE_ORDER) { it.name })
            .map { dir ->
                val (subDirs, subFiles) = countChildren(dir)
                LocalDir(
                    path = dir.absolutePath,
                    name = dir.name,
                    childDirCount = subDirs,
                    childFileCount = subFiles,
                )
            }
    }.getOrDefault(emptyList())

    /**
     * The parent of [path], or null once we reach the top of shared storage.
     *
     * Anything at or above [externalRoot] returns null: there is nothing useful
     * for the user above it, and letting them wander into /data would only offer
     * locations the sync engine cannot write to.
     */
    fun parentOf(path: String): String? {
        val root = normalize(externalRoot())
        val current = normalize(path)
        if (current == root || !current.startsWith("$root/")) return null
        val parent = runCatching { File(current).parent }.getOrNull() ?: return null
        return normalize(parent)
    }

    /**
     * Creates [name] inside [parent] and returns its absolute path.
     *
     * Already existing is success — the user asked for a folder there, and there
     * is one. Failures come back as a sentence that can be shown as-is.
     */
    fun createDir(parent: String, name: String): Result<String> {
        val trimmed = name.trim()
        if (trimmed.isEmpty()) {
            return Result.failure(IllegalArgumentException("Enter a folder name."))
        }
        if (trimmed.contains('/') || trimmed.contains('\\')) {
            return Result.failure(
                IllegalArgumentException("Folder names can’t contain “/” or “\\”."),
            )
        }
        if (trimmed.startsWith(".")) {
            return Result.failure(
                IllegalArgumentException("Folder names can’t start with a dot."),
            )
        }

        return runCatching {
            val target = File(normalize(parent), trimmed)
            if (target.isDirectory) return@runCatching target.absolutePath
            if (target.exists()) {
                throw IllegalStateException("A file called “$trimmed” is already here.")
            }
            if (!target.mkdirs() && !target.isDirectory) {
                throw IllegalStateException("Couldn’t create “$trimmed” here.")
            }
            target.absolutePath
        }.recoverCatching { error ->
            throw IllegalStateException(
                error.message?.takeIf { it.isNotBlank() } ?: "Couldn’t create “$trimmed” here.",
            )
        }
    }

    /** True when files can actually be written into [path]. */
    fun isWritable(path: String): Boolean = runCatching {
        File(normalize(path)).canWrite()
    }.getOrDefault(false)

    /**
     * Human-facing form of [path]: /storage/emulated/0/Nimbo/Testing becomes
     * "Internal storage/Nimbo/Testing".
     */
    fun displayPath(path: String): String {
        val root = normalize(externalRoot())
        val current = normalize(path)
        return when {
            current == root -> ROOT_LABEL
            current.startsWith("$root/") -> ROOT_LABEL + current.removePrefix(root)
            else -> current
        }
    }

    /**
     * Where a remote folder lands by default: [baseDir] plus the remote's own
     * name, so picking /Photos under "Internal storage/Nimbo" suggests
     * "Internal storage/Nimbo/Photos". Syncing the whole account (no segment at
     * all) keeps [baseDir] itself.
     */
    fun defaultLocalFor(baseDir: String, remoteRoot: String): String {
        val base = normalize(baseDir)
        val leaf = remoteRoot.split('/', '\\').lastOrNull { it.isNotBlank() }?.trim()
        return if (leaf.isNullOrEmpty()) base else "$base/$leaf"
    }

    // ---- internals ----------------------------------------------------------

    /** Trims trailing separators (keeping "/" itself) so paths compare and join cleanly. */
    private fun normalize(path: String): String {
        val trimmed = path.trim().trimEnd('/')
        return if (trimmed.isEmpty()) "/" else trimmed
    }

    /**
     * Visible children for a row's subtitle, split into folders and files.
     *
     * Returns (-1, -1) when the directory cannot be read, so the caller stays
     * silent rather than claiming the folder is empty.
     */
    private fun countChildren(dir: File): Pair<Int, Int> = runCatching {
        val entries = dir.listFiles() ?: return@runCatching -1 to -1
        val visible = entries.filter { !it.name.startsWith(".") }
        visible.count { it.isDirectory } to visible.count { it.isFile }
    }.getOrDefault(-1 to -1)
}
