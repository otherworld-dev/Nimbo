<script lang="ts">
  import { Events } from "@wailsio/runtime";
  import { App } from "../bindings/github.com/otherworld/nimbo/cmd/nimbo-gui";

  type Version = { href: string; modified: string; size: number };

  let path = $state("");
  let versions = $state<Version[]>([]);
  let err = $state("");
  let busy = $state(false);
  let loading = $state(true);

  async function load() {
    loading = true;
    path = await App.VersionTarget();
    versions = (await App.VersionList()) ?? [];
    loading = false;
  }
  load();
  Events.On("versions:target", () => { err = ""; load(); });

  const basename = (p: string) => p.replace(/\/+$/, "").split("/").pop() || p;
  function humanBytes(n: number): string {
    if (!n || n < 0) return "—";
    const u = ["B", "KB", "MB", "GB", "TB"];
    let i = 0;
    while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
    return `${n < 10 && i > 0 ? n.toFixed(1) : Math.round(n)} ${u[i]}`;
  }
  async function restore(href: string) {
    err = ""; busy = true;
    const e = await App.RestoreVersion(href);
    busy = false;
    if (e) { err = e; return; }
    await load();
  }
</script>

<div class="win">
  <header>
    <div class="lbl">Version history</div>
    <div class="file" title={path}>📄 {basename(path)}</div>
    <div class="sub">/{path}</div>
  </header>

  <div class="body">
    {#if err}<p class="err">{err}</p>{/if}
    {#if loading}
      <p class="empty">Loading…</p>
    {:else if versions.length === 0}
      <p class="empty">No previous versions. Nextcloud keeps versions as a file changes over time.</p>
    {:else}
      <p class="hint">Restoring makes that version the current file (the present copy is kept as a version too).</p>
      {#each versions as v}
        <div class="ver">
          <div class="vinfo">
            <div class="vdate">{v.modified}</div>
            <div class="vsize">{humanBytes(v.size)}</div>
          </div>
          <button class="primary" onclick={() => restore(v.href)} disabled={busy}>Restore</button>
        </div>
      {/each}
    {/if}
  </div>
</div>

<style>
  .win { height: 100%; display: flex; flex-direction: column; background: var(--bg); color: var(--fg); }
  header { padding: 14px 16px; border-bottom: 1px solid var(--border); }
  .lbl { font-size: 11px; font-weight: 600; letter-spacing: .5px; text-transform: uppercase; color: var(--muted); }
  .file { font-size: 16px; font-weight: 600; margin-top: 2px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .sub { font-size: 12px; color: var(--muted); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .body { flex: 1; overflow-y: auto; padding: 14px 16px; }
  .hint { color: var(--fg2); font-size: 12px; margin: 0 0 12px; }
  .empty { color: var(--muted); font-size: 13px; }
  .err { color: #c0392b; font-size: 12.5px; background: #fdeceb; border: 1px solid #f0c0bb; border-radius: 6px; padding: 8px 10px; margin: 0 0 12px; }
  .ver { display: flex; align-items: center; justify-content: space-between; gap: 10px; padding: 9px 0; border-bottom: 1px solid var(--border-soft); }
  .ver:last-child { border-bottom: none; }
  .vinfo { min-width: 0; }
  .vdate { font-size: 13px; }
  .vsize { font-size: 11.5px; color: var(--muted); }
  .primary { padding: 6px 14px; border: none; border-radius: 6px; background: var(--accent); color: #fff; cursor: pointer; font-size: 12.5px; }
  .primary:hover { background: var(--accent-dark); }
  .primary:disabled { opacity: .6; cursor: default; }
</style>
