# Persistent scan checkpointing (Deck #231) — design

**Date:** 2026-07-23 · **Status:** approved · **Owner:** Adam + Claude

## Problem

`engine.RemoteScanReport` is all-or-nothing: the first directory PROPFIND that
fails poisons the scan and the entire partial result is discarded
([discovery.go:92-96](../../internal/engine/discovery.go), :163-166). That is
correct for the *diff* (a partial remote map would read as mass deletions), but
it means a huge cold crawl that dies mid-way restarts from zero on every
attempt. In the 2026-07-03 incident a ~60k-dir crawl kept dying on server 500s
and re-firing cold for days. The v0.1.0.126 fixes (4 workers, PROPFIND retries,
watch-loop backoff, dev-ignores) treat the symptoms; this feature removes the
restart-from-zero cost.

## Goal

Cache each directory listing as it is fetched, in the per-account state DB.
On a later attempt, a cached listing whose ETag still matches what the parent's
listing reports is used without a network request — chaining down the tree, so
the already-crawled portion of a failed scan costs zero PROPFINDs to re-cover.
The scan's all-or-nothing contract for the diff is unchanged.

## Non-goals

- **Initial clone resume.** `cloneRemote` enumerates via recursive
  (Depth: infinity) PROPFINDs, not this scan path ([agent.go:2161](../../internal/agent/agent.go));
  mid-clone restarts still re-enumerate. Separate card if it ever hurts.
- **The escape probe** ([escape.go:103](../../internal/agent/escape.go)) passes
  nil — rare, user-initiated, user-retryable.
- **Scoped scans.** `computePlanScoped`'s only caller `SyncScope` has no
  callers (dead code); it passes nil. No prefix-scoped clear is built.
- No GUI/bindings changes; observability is log-only.

## Mechanism

### Storage

New table in the per-account state DB, appended to the `schema` const
(`CREATE TABLE IF NOT EXISTS` on every `Open` is the only migration mechanism,
so this column set is final — hence the format-version column):

```sql
CREATE TABLE IF NOT EXISTS scan_checkpoint (
  account_id TEXT    NOT NULL,
  pair_key   TEXT    NOT NULL,
  dir_path   TEXT    NOT NULL,  -- raw files-root-relative path, exactly as queued
  etag       TEXT    NOT NULL,  -- parent-reported ETag at queue time (never '')
  fmt        INTEGER NOT NULL,  -- blob format version; unknown = miss
  saved_at   INTEGER NOT NULL,  -- unix seconds, for age-out
  children   BLOB    NOT NULL,  -- gzip(JSON array of cpEntry), fmt=1
  PRIMARY KEY (account_id, pair_key, dir_path)
);
```

`dir_path` is the raw server-side, files-root-relative form the queue carries
(remote-root prefix embedded, escaped names intact, no slashes at either end).

**Blob format (fmt = 1).** Do NOT marshal `transport.Entry` (no JSON tags — a
field rename would silently orphan rows). A mirror struct stores child *names*,
not full paths (the dominant redundant bytes), with short tags:

```go
type cpEntry struct {
    Name        string `json:"n"`
    IsDir       bool   `json:"d,omitempty"`
    Size        int64  `json:"s,omitempty"`
    ETag        string `json:"e"`
    FileID      string `json:"f,omitempty"`
    Checksums   string `json:"c,omitempty"`
    IsEncrypted bool   `json:"x,omitempty"`
    Permissions string `json:"p,omitempty"`
}
```

Children only — the depth-1 self row is stripped at save (and tolerated
absent). On load, `Entry.Path = dir + "/" + name`; `LastModified`/`ContentType`
are zero — `process()` never consumes them. Gzip + name-only turns the ~250 MB
raw-JSON worst case (700k entries) into tens of MB.

### Engine (`internal/engine/discovery.go`)

Two enabling refactors land first (both mechanical, four call sites):

1. **Options struct.** `RemoteScanReport` is at seven positional params with
   four consecutive nils at one call site. Collapse `RemoteScan`/
   `RemoteScanReport` into
   `RemoteScan(ctx, c PropFinder, root string, opts ScanOpts)` with
   `ScanOpts{Base, Skip, OnEncrypted, Esc, Checkpoint}`.
2. **Client seam.** Narrow `*transport.Client` to
   `type PropFinder interface { PropFind(ctx, path string, depth int) ([]transport.Entry, error) }`
   so resume tests are fast table tests, not an httptest server sleeping
   through real retry backoff.

Checkpoint hook:

```go
type ScanCheckpoint interface {
    // Load returns dir's cached children iff the stored etag == expectedETag.
    // expectedETag == "" never matches. Best-effort: errors read as a miss.
    Load(dir, expectedETag string) ([]transport.Entry, bool)
    // Save stores dir's children under etag. etag == "" is a no-op. Best-effort.
    Save(dir, etag string, children []transport.Entry)
}
```

Queue items become `{dir, etag, noCache}` structs (the four touchpoints:
init [:77], append [:134], pop [:154-155], `process` param). `etag` is the
child's `e.ETag` as reported by its **parent's** listing, captured at the
existing queue site; the root enters with `""`.

`process(item)`:

- If `!item.noCache`, try `cp.Load(item.dir, item.etag)`; on hit, replay the
  cached entries. On miss, `c.PropFind` as today, then
  `cp.Save(item.dir, item.etag, children)` (self row stripped).
- Cached and fresh entries flow through **identical** processing — escape
  decode, `skip()`, E2EE gate, baseline-prune, queueing — so ignore/escape
  rule changes between attempts re-apply naturally (rows are pre-filter raw
  data; a subtree pruned last attempt has no row and fetches fresh if rules
  loosen).
- A child dir is queued with `noCache = item.noCache || strings.Contains(e.Permissions, "M")`.

### Correctness rules (each is load-bearing)

- **Parent-reported ETag, not the self row's.** The queue-time observation
  strictly precedes the child fetch, so any interleaved change makes the stored
  etag stale-*old*: the retry chain reports the newer etag, misses, refetches —
  one wasted request, never a stale hit. Also avoids depending on the depth-1
  self row (only "included first *when present*") and on a self==parent etag
  equivalence nothing in the codebase exercises (every stored dir etag today
  originates from a parent listing).
- **Empty etag never matches, never saves** — on both `Load` and `Save`. Some
  external/SabreDAV backends omit `d:getetag`; without this rule such a
  directory would be cached once and served stale forever (`"" == ""`). This
  also makes the root (queued with `""`) always fetched fresh and never saved,
  which keeps quiet warm passes at zero checkpoint writes.
- **Mount points are never cached** (`M` in `oc:permissions`, inherited by the
  whole subtree via `noCache`). External-storage mounts derive dir etags from
  backend mtimes — the one place the "etag change propagates up" invariant is
  weak. Without this, a cache hit replaying an old listing could feed the diff
  a false "gone from server" for a file that still exists → `ActDeleteLocal`.
  Note the asymmetry this guards: the existing baseline prune fails *safe*
  (it replays the baseline — cannot produce deletions); a checkpoint hit
  replays an old *server* listing and fails unsafe. Cache hits are therefore
  restricted to storage where etag propagation is reliable.
- **All-or-nothing preserved.** Partial results still never reach the diff;
  the checkpoint only cheapens the retry. On failure, rows already saved are
  the whole point — workers that complete PROPFINDs after another worker has
  failed still Save (today that work is silently discarded).
- **Best-effort I/O with a sticky fuse.** The first Load/Save error logs once
  and disables checkpoint I/O for the rest of that scan (no 60k-line error
  spam from a wedged DB). Saves after engine shutdown hit a closed DB —
  harmless, must not panic.

### State (`internal/state/state.go`)

Accessors follow the `CloneStatus` precedent: direct `db` calls, no `s.mu`, no
`cacheEnabled` branch — checkpoint blobs never touch the RAM baseline cache
(that is also the low-memory-mode requirement).

```go
LoadScanDir(pairKey, dir, etag string) (fmt int, blob []byte, ok bool) // WHERE etag=? — no blob decode on mismatch
SaveScanDir(pairKey, dir, etag string, fmt int, blob []byte)           // upsert, saved_at = now
ClearScanCheckpoint(pairKey string)                                    // bounded batches
DeleteScanCheckpointBefore(cutoff time.Time)                           // age-out, account-wide, bounded batches
```

- **Bounded-batch deletes**: `DELETE ... WHERE rowid IN (SELECT rowid ... LIMIT 1000)`
  in a loop — a single multi-hundred-MB DELETE transaction could hold the WAL
  write lock past another process's 5s `busy_timeout` (daemon + CLI are
  separate handles on this file) and fail *their* non-best-effort writes.
- **`RekeyPair`**: add `scan_checkpoint` to the transaction as a **DELETE** of
  the old pair_key's rows (do not migrate — the remote root may have changed;
  the hard-coded `{"baseline", "clone_state"}` list at [state.go:513](../../internal/state/state.go)
  otherwise strands the largest blobs in the DB forever after a folder move).
- No prefix-scoped delete is built (nothing live needs one). If one is ever
  added, copy `queryBaselineScoped`'s `escapeLike + ESCAPE '\'` pattern, not
  `DeleteBaselineUnder`'s unescaped LIKE (latent over-delete bug — paths do
  contain `%`/`_`).

### Agent wiring (`internal/agent`)

A small per-pair handle implements `engine.ScanCheckpoint` over
`getStore() + PairKey`: owns the gzip/JSON codec, the fmt version, hit/miss/save
counters (mutex — 4 workers call in), and the sticky error fuse. The engine
stays log-free; the agent logs one summary after the scan when anything
happened: `scan checkpoint: reused N listings, fetched M, saved K`.

Wired into `computePlan` ([agent.go:1703](../../internal/agent/agent.go)) and
`syncRemoteDelta` ([agent.go:2737](../../internal/agent/agent.go)). Nil for the
escape probe and `computePlanScoped`.

**Clear on clean pass** — a pass that scanned successfully AND applied cleanly
(`err == nil && len(problems) == 0`, same evidence `maintainDirBaselines`
uses). Three hooks, each guarded by an in-memory per-pair dirty flag
(initialized **dirty** at engine start so rows from a previous process life
get cleared; the flag is a per-process hint only — CLI-written rows may linger
until the daemon next dirties, which etag revalidation makes harmless):

1. `applyPlan` quiet path (`len(actions) == 0`, [agent.go:2472-2480](../../internal/agent/agent.go));
2. `applyPlan` executed path, beside the gated `maintainDirBaselines`
   ([agent.go:2550-2551](../../internal/agent/agent.go)), *after* its batch
   commit — WAL ordering then makes "cleared but etags unstamped" impossible;
3. `syncRemoteDelta`'s **no-changes early return**
   ([agent.go:2762-2768](../../internal/agent/agent.go)) — the most common
   clean outcome; a hook only inside `applyPlan` would never fire there.

Also cleared at: `cloneRemote` start (a pair entering clone has no baseline
worth chaining against), `RemoveSyncFolder`, and by the age-out sweep
(`saved_at` older than **14 days**, run once at engine start after `getStore`).

### Lifecycle summary

| Event | Effect on rows |
|---|---|
| Successful PROPFIND of a cacheable dir | upsert row (parent-reported etag) |
| Cache hit | none (row already current) |
| Scan fails / process dies | rows stay (WAL: committed rows survive kill) |
| Clean pass (incl. quiet delta) | pair rows deleted (dirty-flag-guarded) |
| Clone start / folder removed | pair rows deleted |
| Folder moved (`RekeyPair`) | old-key rows deleted in the rekey txn |
| Engine start | age-out sweep (14 days); dirty flag set |

## Known limitations

- A pair whose sync **root sits inside** an external mount is undetectable from
  inside the scan (only the mount node itself carries `M`, and it is above the
  root). Exposure is bounded by the empty-etag rule, the 14-day age-out, and
  the hourly full pass. Documented, accepted.
- The DB file keeps its high-water mark after a big clear (no VACUUM anywhere
  in the codebase; consistent with existing behavior).
- On push-less servers the delta scan runs every 15s; the clear hooks are
  dirty-flag-guarded and the root is never saved, so steady state performs no
  checkpoint writes and no deletes.

## Testing

- **Engine** (table tests via fake `PropFinder` + fake `ScanCheckpoint`):
  resume matrix (fail at dir N, retry, assert only un-cached/changed dirs are
  re-fetched); identical output cached vs fresh; self row present/absent in
  stored listings; empty-etag never hits/saves; `M`-permission subtree never
  cached; loosened ignore rules re-fetch a previously pruned subtree; replay
  after escape-rule change; mid-scan Save failures don't fail the scan.
- **State**: CRUD, etag-mismatch miss without blob decode, bounded-batch
  clear, age-out, RekeyPair deletes old-key rows (extend
  [rekey_test.go](../../internal/state/rekey_test.go)), metachar (`%`/`_`)
  dir_paths, Save on closed store returns without panic.
- **Agent**: clear fires on all three clean hooks and not on dirty passes;
  counters/summary log; unknown `fmt` reads as a miss.

## Implementation order

1. Refactor: `ScanOpts` + `PropFinder` seam (no behavior change; all four call
   sites + tests compile).
2. State: table + accessors + RekeyPair + tests.
3. Engine: queue struct + Load/Save/noCache + tests.
4. Agent: handle/codec, wiring, clear hooks, age-out, logging + tests.
