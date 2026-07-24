# Spec: safe "Move sync folder" via move/sync mutual exclusion

**Status:** approved design, ready to plan · **Date:** 2026-06-30

## Background

The original "Move sync folder" caused **server-side data loss**: it relocated the
local folder while that folder's watcher was still live, so the watcher saw the
files vanish and propagated mass deletions to the server. A first fix added
stop-and-drain ordering plus delete guards, but left one hole: `stopWatcherSync`
waits only 30s for an in-flight sync to drain and then **proceeds with the move
anyway** — and syncs can run for hours. This spec replaces that racy "drain"
approach with a simpler, structurally-safe one.

## Core idea: move and sync are mutually exclusive

A move and a sync can **never overlap**. This makes the disaster's mechanism
(a sync misreading a mid-move folder) impossible by construction, rather than
something we guard against after the fact.

- **A sync is in flight → Move is refused** (and the button is disabled).
- **A move is in progress → syncs are suspended** (the watcher is stopped; new
  local changes / polls / pushes are ignored until the move finishes).

Because a move only ever *starts* when no sync pass is running, stopping the
watcher for the move has **nothing to drain** — the 30s-timeout gamble is gone.

### Mechanism

A single engine-level exclusion (an `RWMutex` used with `TryLock`/`TryRLock`):

- **Sync pass** takes the read side (`TryRLock`). If a move holds the write lock,
  the pass **skips** (it is not queued — the post-move confirming sync reconciles).
- **Move** takes the write side (`TryLock`). If *any* sync pass holds the read
  lock, the move **fails fast**: "a sync is in progress — try again when it
  finishes." No relocate happens.

The acquire-and-check is atomic, so a sync can't slip in between "checked idle"
and "move started."

## Move flow

1. **UI:** Move button is disabled while a sync is active. On click, ask for the
   destination (a new or empty folder).
2. **Engine `MoveSyncFolder(old, new)`:**
   1. `TryLock` the exclusion. If a sync is running → return "sync in progress"
      (UI surfaces it). Otherwise the move now owns the engine.
   2. Stop the pair's watcher (clean — nothing is running).
   3. **Relocate the files** (same-drive rename, or cross-drive copy→verify→delete
      — see below). Report progress.
   4. Re-key the baseline (`RekeyPair`) so the moved folder keeps its synced state.
   5. **Only now** flip config (`pairs[idx].LocalDir = new`, save) to the new path.
   6. Release the exclusion, restart the watcher at the new location.
   7. One confirming sync pass — a no-op when all is well.
3. On any failure before step 5, config is untouched and the watcher restarts at
   the **original** location.

## Relocate strategy

`relocateFolder(src, dst)` — destination must be new/empty (a non-empty dst is
still refused, as today):

- **Same drive → atomic `os.Rename`.** The data is never copied or deleted, only
  re-labelled; the op is atomic (fully succeeds or fails with nothing changed),
  instant, and needs no extra disk space. This is both the fastest *and* the
  safest path — there is no window in which data is at risk.
- **Cross drive → copy → verify → delete source.** A rename can't cross volumes,
  so:
  1. Pre-check the destination has enough free space.
  2. Copy the tree (preserving mtimes so moved files don't read as edits).
  3. **Verify** the copy: file count and per-file sizes at dst match src.
  4. **Only if verification passes**, delete the source.
  5. If the copy or verification fails → **keep the source**, remove the partial
     dst, surface the error. The original is never deleted until the copy is
     proven complete and correct.

## Crash safety

Config is flipped to the new path **only after** the relocate (and verify)
completes:

- Crash during a **cross-drive copy** → source intact, config still points at the
  source, dst is a discardable partial. Nimbo resumes the original folder.
- Crash during/just after a **same-drive rename** → rename is atomic; the only
  narrow window is "renamed but config not yet saved," in which the files are
  intact at the new path and the existing `localRootVanished` guard prevents any
  server deletion. Recoverable, no data loss.

## UI / status

- **Move disabled while sync active**; re-checked at click time by the engine's
  `TryLock` (a stale UI can't sneak a move through).
- **Move status shown in the flyout** — "Moving your folder… N%" with real copy
  progress on a cross-drive move; effectively instant on a same-drive rename.
  Reuse the existing `Status()` / `Progress()` plumbing (add a `moving` flag and
  reuse the byte counters) so **no new Wails binding** is needed — avoids a
  bindings-id regen.
- While a move runs, the pause/sync controls reflect "paused for move."

## What changes vs today

- **Removed:** the 30s drain-then-proceed timeout in `stopWatcherSync` as the
  move's safety mechanism (mutual exclusion replaces it; `stopWatcherSync` may
  remain for other callers but the move no longer relies on draining a *running*
  sync, because none can be running).
- **Kept as backstop (defence in depth):** `localRootVanished` and
  `bulkDeleteGuardTrips`. They are no longer the primary protection, but stay.
- **Added:** cross-drive copy **verification** before deleting the source; a
  free-space pre-check; move progress in the flyout.

## Freeze scope (decided)

During a move, **all** syncing is frozen, not just the moved pair — simplest, and
moves are rare (the common setup is a single whole-account folder).

## Test plan (data-safety-critical → thorough)

Unit tests (no real server, no real data):

1. **Mutual exclusion — the disaster reproduction.** With a sync pass marked
   in-flight, starting a move **fails fast** and performs **no relocate**. With a
   move in progress, a sync pass **skips** (no actions applied). This is the test
   that proves the original bug cannot recur.
2. **Orchestration order.** `MoveSyncFolder` stops the watcher *before* relocate,
   re-keys, and flips config *after* relocate; on relocate failure config is
   unchanged and the original watcher is restored.
3. **Same-drive uses rename; cross-drive uses copy→verify→delete.** Simulated
   cross-volume (rename refused) path copies, verifies, then deletes source.
4. **Verification gate.** A short/corrupted cross-drive copy (size mismatch) →
   source **preserved**, partial dst removed, error returned. Source is never
   deleted on a failed/again-unverified copy.
5. **Free-space pre-check** refuses a cross-drive move that wouldn't fit.
6. Existing `localRootVanished` / `bulkDeleteGuardTrips` tests stay green.

Manual, on a throwaway pair:

- Same-drive move → instant, "Up to date" after, no churn, no server deletions.
- Cross-drive move → flyout shows copy progress; source removed only after; resync
  is a no-op.
- Click Move mid-sync → button disabled / engine refuses with a clear message.

## Build order (for the plan)

1. Engine exclusion (`TryLock`/`TryRLock`) + sync passes honour it; unit test #1.
2. Rework `MoveSyncPair` to the new flow (lock → stop watcher → relocate → re-key
   → flip config → unlock → restart → confirm); unit tests #2.
3. Relocate: keep same-drive rename; add cross-drive copy→**verify**→delete +
   free-space pre-check; unit tests #3–#5.
4. Move status in the flyout via existing Status/Progress plumbing (no new
   binding).
5. UI: disable Move while sync active; reflect "paused for move."
6. Full test pass + a manual same-drive and cross-drive move on a throwaway pair.
