<script lang="ts">
  // A server folder tree with tickboxes, for choosing which folders sync.
  //
  // It only STAGES changes: clicks edit `target` (the excludes after the
  // user's changes); whoever hosts the picker compares it with `committed`
  // (the excludes saved now) and applies the difference, see planChanges in
  // folderSelection.ts. Folders load one level at a time as they're opened.
  // Paths in committed/target are relative to `root`.
  import { App } from "../bindings/github.com/otherworld/nimbo/cmd/nimbo-gui";
  import Icon from "./Icon.svelte";
  import { tickState, toggle, planChanges, rowChange, fmtSize, type Tick } from "./folderSelection";

  type Entry = { name: string; path: string; isDir: boolean; size?: number };
  type Node = { name: string; rel: string; size: number };

  let {
    root,
    committed,
    target = $bindable(),
    sizes = $bindable({}),
    disabled = false,
    browse = (p: string) => App.BrowseRemote(p) as Promise<Entry[] | null>,
  }: {
    root: string;                        // server folder the paths are relative to ("" = whole account)
    committed: string[];                 // unticked folders as saved now
    target: string[];                    // unticked folders after the staged clicks
    sizes?: Record<string, number>;      // server size of every folder loaded so far, by path
    disabled?: boolean;
    browse?: (remotePath: string) => Promise<Entry[] | null>;
  } = $props();

  const trim = (s: string) => s.replace(/^\/+|\/+$/g, "");
  const base = $derived(trim(root));
  const remoteOf = (rel: string) => (base && rel ? base + "/" + rel : base || rel);
  const relOf = (p: string) => (base === "" ? trim(p) : trim(p).slice(base.length + 1));

  // Children by folder path ("" = the root), and the per-folder load state.
  let kids = $state<Record<string, Node[]>>({});
  let loading = $state<Record<string, boolean>>({});
  let failed = $state<Record<string, boolean>>({});
  let open = $state<Record<string, boolean>>({});

  async function load(rel: string) {
    if (kids[rel] || loading[rel]) return;
    loading[rel] = true;
    failed[rel] = false;
    try {
      // BrowseRemote answers null when the listing failed and [] when the
      // folder really has nothing in it.
      const list = await browse(remoteOf(rel));
      if (!list) { failed[rel] = true; return; }
      const nodes = list.filter(e => e.isDir).map(e => ({ name: e.name, rel: relOf(e.path), size: e.size ?? 0 }));
      kids[rel] = nodes;
      sizes = { ...sizes, ...Object.fromEntries(nodes.map(n => [n.rel, n.size])) };
    } catch {
      failed[rel] = true;
    } finally {
      loading[rel] = false;
    }
  }
  load("");

  function toggleOpen(rel: string) {
    open[rel] = !open[rel];
    if (open[rel]) load(rel);
  }

  const plan = $derived(planChanges(target, committed));

  type Row =
    | { kind: "folder"; node: Node; depth: number; tick: Tick; change: ReturnType<typeof rowChange> }
    | { kind: "note"; rel: string; depth: number; text: string; retry: boolean };
  const rows = $derived.by(() => {
    const out: Row[] = [];
    const walk = (rel: string, depth: number) => {
      for (const node of kids[rel] ?? []) {
        out.push({ kind: "folder", node, depth, tick: tickState(node.rel, target), change: rowChange(node.rel, plan) });
        if (!open[node.rel]) continue;
        if (loading[node.rel]) out.push({ kind: "note", rel: node.rel, depth: depth + 1, text: "Loading…", retry: false });
        else if (failed[node.rel]) out.push({ kind: "note", rel: node.rel, depth: depth + 1, text: "Couldn’t load this folder.", retry: true });
        else if (kids[node.rel]?.length === 0) out.push({ kind: "note", rel: node.rel, depth: depth + 1, text: "No folders inside", retry: false });
        else walk(node.rel, depth + 1);
      }
    };
    walk("", 0);
    return out;
  });

  // A tickbox has three looks; "indeterminate" is a DOM property, not an
  // attribute, so it is set here. Setting `checked` here too keeps the box
  // right when a click lands on the same look it had (off → half-ticked).
  function tri(el: HTMLInputElement, t: Tick) {
    const set = (t: Tick) => { el.checked = t === "on"; el.indeterminate = t === "partial"; };
    set(t);
    return { update: set };
  }
  function onTick(rel: string, el: HTMLInputElement) {
    target = toggle(rel, target, committed);
    const t = tickState(rel, target);
    el.checked = t === "on";
    el.indeterminate = t === "partial";
  }

  const CHANGE_TEXT = { stops: "Won’t sync", starts: "Will sync", inside: "Changes inside", "": "" } as const;
  function tickTitle(t: Tick, name: string): string {
    if (t === "inherited") return "Inside a folder that isn’t syncing. Tick that folder first to choose in here.";
    if (t === "partial") return `Some folders inside “${name}” don’t sync. Click to sync all of it.`;
    if (t === "off") return `“${name}” doesn’t sync. Click to sync it.`;
    return `Click to stop syncing “${name}”.`;
  }
</script>

<div class="tree">
  {#if loading[""]}
    <p class="note">Loading…</p>
  {:else if failed[""]}
    <p class="note">Couldn’t load your folders. <button class="link" onclick={() => load("")}>Try again</button></p>
  {:else if (kids[""] ?? []).length === 0}
    <p class="note">There are no folders in here.</p>
  {/if}
  {#each rows as r (r.kind + ":" + (r.kind === "folder" ? r.node.rel : r.rel))}
    {#if r.kind === "folder"}
      {@const n = r.node}
      {@const leaf = kids[n.rel]?.length === 0}
      <div class="trow" class:stops={r.change === "stops"} class:starts={r.change === "starts"} class:inherited={r.tick === "inherited"}
           style="padding-left: {4 + r.depth * 20}px">
        {#if leaf}
          <span class="chev" aria-hidden="true"></span>
        {:else}
          <button class="chev" class:open={open[n.rel]} aria-expanded={!!open[n.rel]} aria-label={(open[n.rel] ? "Hide" : "Show") + " folders in " + n.name}
                  onclick={() => toggleOpen(n.rel)}><Icon name="chevron" size={14} /></button>
        {/if}
        <input type="checkbox" class="tick" use:tri={r.tick} aria-label={n.name} title={tickTitle(r.tick, n.name)}
               disabled={disabled || r.tick === "inherited"} onchange={(e) => onTick(n.rel, e.currentTarget)} />
        <button class="tname" title={n.name} onclick={() => !leaf && toggleOpen(n.rel)} class:leaf>
          <span class="ficon"><Icon name="folder" /></span>
          <span class="tlabel">{n.name}</span>
        </button>
        {#if r.change}<span class="chg {r.change}">{CHANGE_TEXT[r.change]}</span>{/if}
        {#if n.size > 0}<span class="size">{fmtSize(n.size)}</span>{/if}
      </div>
    {:else}
      <p class="note" style="padding-left: {4 + r.depth * 20 + 24}px">
        {r.text}{#if r.retry}{" "}<button class="link" onclick={() => load(r.rel)}>Try again</button>{/if}
      </p>
    {/if}
  {/each}
</div>

<style>
  .tree { border: 1px solid var(--border); border-radius: 8px; background: var(--panel); padding: 4px 0; }
  .trow { display: flex; align-items: center; gap: 4px; padding: 2px 8px 2px 4px; min-height: 28px; }
  .trow:hover { background: var(--hover); }
  .trow.stops, .trow.starts { background: var(--tint); }
  .trow.inherited .tname { color: var(--muted); }
  .chev { flex: 0 0 auto; width: 20px; height: 20px; display: inline-flex; align-items: center; justify-content: center;
          border: none; background: none; padding: 0; border-radius: 4px; color: var(--fg2); cursor: pointer; }
  button.chev:hover { background: var(--panel-2); color: var(--fg); }
  .chev :global(svg) { transition: transform .12s ease; }
  .chev.open :global(svg) { transform: rotate(90deg); }
  .tick { flex: 0 0 auto; margin: 0 4px 0 2px; accent-color: var(--accent); }
  .tname { flex: 1; min-width: 0; display: flex; align-items: center; gap: 6px; border: none; background: none; padding: 2px 0;
           font-size: 13px; color: var(--fg); text-align: left; cursor: pointer; }
  .tname.leaf { cursor: default; }
  .tname:not(.leaf):hover .tlabel { color: var(--accent); }
  .ficon { color: var(--accent); display: inline-flex; }
  .tlabel { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .chg { flex: 0 0 auto; font-size: 11.5px; white-space: nowrap; }
  .chg.stops { color: #b26a00; }
  .chg.starts { color: #2e7d32; }
  .chg.inside { color: var(--muted); }
  .size { flex: 0 0 auto; min-width: 52px; text-align: right; font-size: 12px; color: var(--muted); font-variant-numeric: tabular-nums; }
  .note { margin: 0; padding: 4px 10px; font-size: 12px; color: var(--muted); }
  .link { background: none; border: none; color: var(--accent); cursor: pointer; padding: 0; font-size: inherit; }
</style>
