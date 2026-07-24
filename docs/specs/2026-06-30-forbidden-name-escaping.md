# Spec: sync server-forbidden filenames via opt-in name escaping

**Status:** approved design, ready to plan · **Date:** 2026-06-30

## Goal

Let users sync files whose names the Nextcloud server forbids (e.g. `.htaccess`)
by transparently storing them under an allowed name on the server and decoding
them back locally. **Opt-in by file type (extension)** — the user explicitly
chooses which blocked types to escape.

## Scheme

A forbidden local file `X` is stored on the server as **`X.nimboesc`** (append a
`.nimboesc` suffix to the basename); on download it is decoded back to `X`. Chosen
over a `nimbo.` prefix because:

- the original name stays legible (`.htaccess.nimboesc` reads clearly in the web UI),
- decode is an unambiguous suffix-strip,
- it changes the *extension* to `nimboesc`, which servers don't forbid, and
- `.nimboesc` collides with a real file far less often than a short/common marker.

For white-label builds the suffix tracks the brand (e.g. `.acmeesc`) or stays a
neutral marker.

## Scope

- **In:** forbidden **names / extensions** matched by file extension (what blocks
  `.htaccess`, `.htpasswd`, blocked extensions) — the actual use case.
- **Out:** forbidden **characters** (a `:` in the name isn't fixed by a suffix;
  needs character-level escaping — separate future work). Those stay blocked.
- **Out:** forbidden **basenames** that aren't expressible as an extension stay
  blocked (the opt-in list is extension-keyed by decision).
- **Not a silent policy bypass:** opt-in, and the **admin policy layer can
  force-disable** it for managed/white-label deployments where the host's block is
  intentional.

## Why this is low-risk (the important part)

Code exploration confirmed Nimbo's diff, baseline, rename-detection, conflict
resolution, and the bulk-delete circuit-breaker **all key off LOCAL names**. The
server name appears only at the **transport edge**. So escaping is two isolated
translations and the data-safety-critical core is **untouched**:

- the diff never sees `X.nimboesc` (decoded before the remote map is built),
- the bulk-delete guard and conflict handling apply unchanged.

## Design

### Config: the escape list (opt-in, by extension)

A persisted set of **extensions** in settings, e.g. `[".htaccess", ".htpasswd"]`
(lowercased, leading dot; `.htaccess` is its own extension per `filepath.Ext`).
Empty by default — nothing is escaped until the user opts in.

### The Escaper (new, pure, heavily tested)

Built from the existing `engine.Forbidden` matcher + the escape list:

- `Encode(localRel) -> serverRel` — append `.nimboesc` to the basename **iff** the
  name is forbidden AND its extension is on the escape list. Deterministic,
  idempotent.
- `Decode(serverRel) -> (localRel, wasEscaped)` — strip a trailing `.nimboesc`
  **iff** the decoded name's extension is on the escape list (two-condition check,
  so a genuine `*.nimboesc` file whose type isn't opted-in is left alone).

### Two hook points (transport edge only)

1. **Decode — building the remote map** (`engine.RemoteScanReport`, after the
   pair-relative `rel` is computed): `rel, _ = esc.Decode(rel)`. The remote map is
   then keyed by LOCAL names, so the diff matches `X` <-> `X` as normal.
2. **Encode — deriving the server path** (`transfer.Executor.remotePath(rel)`):
   return `esc.Encode(joined)`. This one function feeds PUT (upload), DELETE,
   MOVE (rename) and MKCOL — so escaping is consistent across all server ops.

### Blocking decision

`FilterBlocked` currently surfaces forbidden uploads as "blocked". A forbidden
name whose extension is **on the escape list** is no longer blocked — it proceeds
and the executor escapes it. Anything else forbidden is blocked as today. (The
escape list is a new input to the block decision.)

### Config + UI (opt-in, discoverable)

Surfaced on the existing **Blocked tab**: next to a blocked `.htaccess`, a button
"Sync `.htaccess` files (rename on server)" adds that **extension** to the escape
list. The user opts in from the thing that's bothering them and sees what's happening.

### Un-escape / disable migration (built in v1)

Disabling a type (removing its extension, or a "Stop escaping `.htaccess`" action)
runs a **confirmed, direct** migration — NOT through the sync diff, so it never
fights the bulk-delete guard:

1. Confirm every `X.nimboesc` of that type is present locally as `X` (it is, via
   decode) — re-download any that aren't first.
2. **Delete the `X.nimboesc` copies from the server** directly (the server can't
   hold them under the real name).
3. The files become device-only again (re-surface as "blocked").

Clear warning up front: "N files will stop syncing and remain on this device only
— the server forbids their names." No data is lost locally.

### Untouched

`diff.go`, `state.go` (baseline), `rename.go`, conflict resolution, the
circuit-breaker — no changes; all stay in local names.

## Edge cases & handling

1. **Genuine `*.nimboesc` file** — decode only un-suffixes names whose decoded
   extension is on the escape list, so a real `data.nimboesc` (type not opted-in)
   is left alone.
2. **Idempotency / double-suffix** — encode acts only on LOCAL names (forbidden
   `X`); the local side never holds `X.nimboesc` as a synced name, so no
   `X.nimboesc.nimboesc`.
3. **Collision: local has both forbidden `X` and a real `X.nimboesc`** — encoding
   `X` would clash. Detect at encode time; if a *different* file already maps to
   `X.nimboesc`, fall back to **blocking `X`** rather than overwrite. (Rare.)
4. **Orphaned escape (enabled mid-use, leftover `X.nimboesc`, no baseline)** —
   decodes to a "new" remote `X`; existing conflict/adopt logic handles remote `X`
   vs local `X` (adopt if identical, keep-both if different) — never a blind
   clobber; the bulk-delete guard backstops anything worse.
5. **Interop** — other clients (web/official/mobile) see `X.nimboesc`. Documented
   limitation; the value is "files sync at all" against a host that forbids the name.

## Testing (data-safety-critical -> thorough)

- **Round-trip unit tests** on the Escaper: `Decode(Encode(X)) == X` across the
  forbidden set and opted-in extensions; `Encode` idempotent; `Decode` leaves
  non-opted-in / non-escaped names untouched; collision fallback.
- An integration-style test: a forbidden file yields an Upload that, after Encode,
  targets `X.nimboesc`; a remote `X.nimboesc` decodes so the diff sees **no change**
  (no spurious upload or delete).
- Manual on a real server: opt in `.htaccess`, confirm it uploads as
  `.htaccess.nimboesc`, shows locally as `.htaccess`, edits round-trip, delete
  round-trips, a re-sync produces no churn, and the disable-migration cleans up.

## Decisions (from review)

1. Suffix: **`.nimboesc`** (less guessable than `.nimbo`).
2. Escape list granularity: **by extension**.
3. Un-escape/disable migration: **build in v1** (direct, confirmed; removes the
   escaped server copies, files revert to device-only).
4. Admin **policy layer can force-disable** escaping (white-label/managed).

## Build order (for the plan)

1. Escaper module + exhaustive round-trip/idempotency/collision unit tests.
2. Settings field (escape list) + policy gate.
3. Wire the two hooks (decode in RemoteScanReport, encode in Executor.remotePath);
   teach FilterBlocked the escape list.
4. Disable-migration (direct server delete + local-presence check).
5. Blocked-tab UI (opt-in button) + binding regen.
6. Build, test, and verify on a throwaway forbidden file before any release.
