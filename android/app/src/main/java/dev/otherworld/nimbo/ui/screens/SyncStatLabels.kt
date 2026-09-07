/*
 * SyncStatLabels.kt — turns a sync pass's counters into the chips shown on the
 * "Last sync" card.
 *
 * Deliberately plain Kotlin (no Compose imports) so the wording is unit-testable
 * on the JVM. The card previously hard-coded "N down" and "N up", so a pass whose
 * work was a deletion, a move or a folder creation reported "0 down, 0 up" —
 * identical to having done nothing. Every non-zero counter now earns a chip.
 */
package dev.otherworld.nimbo.ui.screens

import dev.otherworld.nimbo.core.SyncStats

/** One chip: [isProblem] marks conflicts and failures for error colouring. */
data class StatLabel(val text: String, val isProblem: Boolean = false)

/** "1 file" / "2 files" — the count with a correctly pluralised noun. */
private fun count(n: Long, singular: String, plural: String = singular + "s"): String =
    "$n " + if (n == 1L) singular else plural

/**
 * Chips for every non-zero counter, in the order a person would read them:
 * what moved, then what was removed, then what went wrong.
 */
fun syncStatLabels(stats: SyncStats): List<StatLabel> {
    val out = mutableListOf<StatLabel>()
    if (stats.downloaded > 0) out += StatLabel("${stats.downloaded} downloaded")
    if (stats.uploaded > 0) out += StatLabel("${stats.uploaded} uploaded")
    if (stats.mkLocal > 0) out += StatLabel(count(stats.mkLocal, "folder") + " here")
    if (stats.mkRemote > 0) out += StatLabel(count(stats.mkRemote, "folder") + " on server")
    if (stats.moved > 0) out += StatLabel("${stats.moved} moved")
    if (stats.delLocal > 0) out += StatLabel("${stats.delLocal} deleted here")
    if (stats.delRemote > 0) out += StatLabel("${stats.delRemote} deleted on server")
    if (stats.conflictsIdentical > 0) out += StatLabel("${stats.conflictsIdentical} identical")
    if (stats.conflictsResurrected > 0) out += StatLabel("${stats.conflictsResurrected} restored")
    if (stats.conflicts > 0) {
        out += StatLabel(count(stats.conflicts, "conflict"), isProblem = true)
    }
    if (stats.failed > 0) out += StatLabel("${stats.failed} failed", isProblem = true)
    if (out.isEmpty()) out += StatLabel("Nothing to do")
    return out
}
