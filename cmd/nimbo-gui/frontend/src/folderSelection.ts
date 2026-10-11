// Tick state and staged changes for the folder picker (Settings → Folders →
// Choose folders…). Kept free of Svelte and of the bindings so the rules can be
// tested on their own (folderSelection.test.ts).
//
// Everything works on pair-relative folder paths ("Photos/2024"), the same
// strings the sync pair stores as its excludes. Two lists describe a session:
//
//   committed  the excludes saved right now;
//   target     the excludes after the user's staged (not yet applied) clicks.
//
// Nothing is sent to the engine until Apply, which turns the difference
// between the two into calls (planChanges).

export type Tick =
  | "on"        // syncs, and everything inside it does
  | "partial"   // syncs, but some folder inside it doesn't
  | "off"       // unticked: doesn't sync
  | "inherited"; // inside an unticked folder: can't be changed on its own

/** x is strictly inside folder p. */
export const isInside = (x: string, p: string): boolean => x.startsWith(p + "/");

export function tickState(rel: string, excludes: readonly string[]): Tick {
  if (excludes.some(x => isInside(rel, x))) return "inherited";
  if (excludes.includes(rel)) return "off";
  return excludes.some(x => isInside(x, rel)) ? "partial" : "on";
}

const uniq = (xs: string[]) => [...new Set(xs)];

/**
 * One click on rel's tickbox. Returns the new target.
 *
 *   on       → off: rel stops syncing. Anything staged inside it is dropped
 *              and the saved excludes inside it come back, so unticking a
 *              folder never also sends calls for the folders in it.
 *   partial  → on:  "sync all of it": every exclude inside rel is cleared.
 *   off      → back on, keeping whatever was already unticked inside it (so
 *              off then on again is no change at all). A folder that had
 *              unticked folders inside shows half-ticked; one more click
 *              syncs all of it.
 *   inherited: no change (the tickbox is disabled).
 */
export function toggle(rel: string, target: readonly string[], committed: readonly string[]): string[] {
  switch (tickState(rel, target)) {
    case "on":
      return uniq([
        ...target.filter(x => !isInside(x, rel)),
        ...committed.filter(x => isInside(x, rel)),
        rel,
      ]);
    case "partial":
      return target.filter(x => !isInside(x, rel));
    case "off":
      return target.filter(x => x !== rel);
    default:
      return [...target];
  }
}

export type Plan = {
  exclude: string[]; // stop syncing: DeselectFolder, one keep/delete answer for all
  include: string[]; // sync again: RemoveExclude
};

/**
 * The calls Apply makes. A new exclude inside another new exclude is dropped:
 * the outer one already covers it (and with "delete" it already removes the
 * files), so it would only be a second call for the same thing.
 */
export function planChanges(target: readonly string[], committed: readonly string[]): Plan {
  const c = new Set(committed);
  const t = new Set(target);
  const added = uniq(target.filter(x => !c.has(x)));
  return {
    exclude: added.filter(x => !added.some(y => isInside(x, y))).sort(),
    include: uniq(committed.filter(x => !t.has(x))).sort(),
  };
}

export const planEmpty = (p: Plan) => p.exclude.length === 0 && p.include.length === 0;

/**
 * Whether Apply has to ask "keep or delete the downloaded files?". Not when
 * every folder that stops syncing sits inside a folder that wasn't syncing
 * anyway (re-tick a folder, untick one inside it): nothing of it is on this PC.
 */
export const asksKeepOrDelete = (plan: Plan, committed: readonly string[]): boolean =>
  plan.exclude.some(x => !committed.some(c => isInside(x, c)));

/** The outermost of the given folders that sit inside rel. */
function topInside(rel: string, xs: readonly string[]): string[] {
  const inside = xs.filter(x => isInside(x, rel));
  return inside.filter(x => !inside.some(y => isInside(x, y)));
}

/**
 * Roughly how much disk space Apply frees if the local copies are deleted:
 * the server size of each folder that stops syncing, less what was already
 * not on this PC inside it. Sizes not loaded yet count as 0.
 */
export function freedBytes(plan: Plan, committed: readonly string[], sizes: Record<string, number>): number {
  let n = 0;
  for (const x of plan.exclude) {
    let s = sizes[x] ?? 0;
    for (const y of topInside(x, committed)) s -= sizes[y] ?? 0;
    n += Math.max(0, s);
  }
  return n;
}

/** Roughly how much Apply downloads: each folder that syncs again, less what stays unticked inside it. */
export function downloadBytes(plan: Plan, target: readonly string[], sizes: Record<string, number>): number {
  let n = 0;
  for (const x of plan.include) {
    let s = sizes[x] ?? 0;
    for (const y of topInside(x, target)) s -= sizes[y] ?? 0;
    n += Math.max(0, s);
  }
  return n;
}

/** What a row shows about its staged change, or "" when it has none. */
export function rowChange(rel: string, plan: Plan): "" | "stops" | "starts" | "inside" {
  if (plan.exclude.includes(rel)) return "stops";
  if (plan.include.includes(rel)) return "starts";
  if (plan.exclude.some(x => isInside(x, rel)) || plan.include.some(x => isInside(x, rel))) return "inside";
  return "";
}

/** Bytes as a short size: "0 B", "340 KB", "4.2 MB", "12 GB". */
export function fmtSize(bytes: number): string {
  if (!bytes || bytes < 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let v = bytes;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  const digits = i === 0 || v >= 10 ? 0 : 1;
  return v.toFixed(digits) + " " + units[i];
}
