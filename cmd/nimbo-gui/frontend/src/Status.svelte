<script lang="ts">
  import { Events } from "@wailsio/runtime";
  import { App } from "../bindings/github.com/otherworld/nimbo/cmd/nimbo-gui";

  type Activity = { time: string; kind: string; path: string; err: string; account: string };
  type OtherAttention = { id: string; user: string; conflicts: number; blocked: number };
  type Conflict = {
    localDir: string; path: string; kind: string;
    localExists: boolean; remoteExists: boolean;
    localSize: number; localMTime: string; remoteSize: number; remoteMTime: string;
  };
  type Notif = { id: number; app: string; subject: string; message: string; link: string; actions: { label: string }[] };
  type Blocked = { abs: string; path: string; reason: string; ext: string; escapable: boolean; escaping: boolean };
  type Trash = { href: string; name: string; originalLocation: string; deletedAt: string; size: number; isDir: boolean };

  let tab = $state<"activity" | "conflicts" | "notifications" | "blocked" | "trash">("activity");
  let activity = $state<Activity[]>([]);
  let conflicts = $state<Conflict[]>([]);
  let notifs = $state<Notif[]>([]);
  let blocked = $state<Blocked[]>([]);
  let trash = $state<Trash[]>([]);
  let trashBusy = $state(false);
  let otherAttn = $state<OtherAttention[]>([]);

  async function loadActivity() { activity = (await App.RecentActivity()) ?? []; }
  async function loadConflicts() {
    conflicts = (await App.Conflicts()) ?? [];
    otherAttn = (await App.OtherAccountAttention()) ?? [];
  }
  async function showAccount(id: string) {
    const err = await App.SwitchAccount(id);
    if (err) { alert(err); return; }
    loadAll();
  }
  async function loadNotifs() { notifs = (await App.NotificationList()) ?? []; }
  // cast: the Go BlockedItem carries ext/escapable/escaping (added without a
  // bindings regen), so the generated type is a subset of our Blocked at runtime.
  async function loadBlocked() { blocked = ((await App.BlockedList()) ?? []) as unknown as Blocked[]; }
  async function loadTrash() { trashBusy = true; trash = (await App.TrashList()) ?? []; trashBusy = false; }
  function loadAll() { loadActivity(); loadConflicts(); loadNotifs(); loadBlocked(); }
  loadAll();

  const restoreTrash = async (t: Trash) => { await App.RestoreTrash(t.href); await loadTrash(); };
  const deleteTrash = async (t: Trash) => { await App.DeleteTrash(t.href); await loadTrash(); };

  // Open on the tab the caller requested (e.g. the flyout bell → Notifications),
  // and switch live if the window was already open. A deep-link is either the
  // bare "<tab>" or "<tab>" + newline + "<highlight-key>": the flyout newline-
  // joins the target row's key into OpenStatusTab's one string arg (so no extra
  // Go binding is needed), and we flash + scroll to the matching row.
  type Tab = "activity" | "conflicts" | "notifications" | "blocked" | "trash";
  const NL = String.fromCharCode(10); // newline — never occurs in a tab name or file path
  let highlight = $state("");
  let hlTimer: ReturnType<typeof setTimeout>;
  function flashHighlight(hi: string) {
    highlight = "";                              // restart the flash if the same row is re-picked
    requestAnimationFrame(() => { highlight = hi; });
    clearTimeout(hlTimer);
    hlTimer = setTimeout(() => { highlight = ""; }, 3500);
  }
  function applyDeepLink(raw: string) {
    if (!raw) return;
    const i = raw.indexOf(NL);
    const t = (i >= 0 ? raw.slice(0, i) : raw) as Tab;
    const hi = i >= 0 ? raw.slice(i + 1) : "";
    if (t) { tab = t; if (t === "trash") loadTrash(); }
    if (hi) flashHighlight(hi);
  }
  (async () => { applyDeepLink((await App.InitialStatusTab()) as string); })();
  Events.On("status-tab", (e: any) => applyDeepLink(e.data as string));
  // Once the active tab's list has rendered, scroll the flashed row into view.
  $effect(() => {
    void (activity.length + conflicts.length + notifs.length + blocked.length + trash.length);
    if (!highlight) return;
    requestAnimationFrame(() =>
      document.querySelector(".body .hl")?.scrollIntoView({ block: "center", behavior: "smooth" }));
  });

  Events.On("activity", loadActivity);
  Events.On("conflicts", loadConflicts);
  Events.On("notifications", loadNotifs);
  Events.On("blocked", loadBlocked);

  const conflictDesc = (k: string) =>
    k === "deleted-locally" ? "You deleted this; it changed on the server."
    : k === "deleted-remotely" ? "Deleted on the server; you changed it here."
    : k === "type" ? "Changed to a different type on each side."
    : "Edited on both sides since the last sync.";

  const resolve = (c: Conflict, choice: string) => App.ResolveConflict(c.localDir, c.path, choice);

  // Content preview, lazily fetched when a conflict is expanded.
  type Side = { exists: boolean; size: number; isText: boolean; preview: string; truncated: boolean; note: string };
  type Preview = { local: Side; remote: Side };
  let previews = $state<Record<string, Preview>>({});
  let previewOpen = $state<Record<string, boolean>>({});
  const ckey = (c: Conflict) => `${c.localDir}|${c.path}`;
  async function togglePreview(c: Conflict) {
    const k = ckey(c);
    previewOpen[k] = !previewOpen[k];
    if (previewOpen[k] && !previews[k]) {
      previews[k] = await App.ConflictPreview(c.localDir, c.path) as Preview;
    }
  }

  function humanBytes(n: number): string {
    if (!n || n < 0) return "—";
    const u = ["B", "KB", "MB", "GB", "TB"];
    let i = 0;
    while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
    return `${n < 10 && i > 0 ? n.toFixed(1) : Math.round(n)} ${u[i]}`;
  }
  const newer = (c: Conflict): "local" | "remote" | "" =>
    !c.localMTime || !c.remoteMTime ? "" : c.localMTime > c.remoteMTime ? "local" : c.remoteMTime > c.localMTime ? "remote" : "";

  function rename(b: Blocked) {
    const base = b.abs.split(/[\\/]/).pop() ?? b.path;
    const name = prompt("Rename to a name the server allows:", base);
    if (name) App.RenameBlocked(b.abs, name);
  }
  function deleteBlocked(b: Blocked) {
    if (confirm(`Delete "${b.path}"?\n\nIt can't sync because of its name and isn't on the server, so this just removes it from this device.`)) {
      App.DeleteBlocked(b.abs);
    }
  }
  async function deleteAllBlocked() {
    const n = realBlocked.length;
    if (confirm(`Delete all ${n} can't-sync file${n === 1 ? "" : "s"} from this device?\n\nThey can't sync because of their names and aren't on the server, so this only removes them locally.`)) {
      await App.DeleteAllBlocked();
      loadBlocked();
    }
  }
  // Real blocked files (excludes the synthetic "currently escaping" rows).
  const realBlocked = $derived(blocked.filter((b) => !b.escaping));
  // Opt this file's type into escaping: it'll sync stored under a renamed copy.
  function syncEscape(b: Blocked) {
    App.RenameBlocked(b.abs, "//escape");
    loadBlocked();
  }
  // Stop escaping a type: removes the renamed server copies; files go device-only.
  function stopEscape(b: Blocked) {
    if (confirm(`Stop syncing ${b.ext} files?\n\nTheir renamed copies are removed from the server and the files stay on this device only — the server forbids their real names. Nothing is deleted locally.`)) {
      App.RenameBlocked("", "//unescape:" + b.ext);
    }
  }
  async function dismissAllNotifs() {
    await App.DismissAllNotifications();
    loadNotifs();
  }
</script>

<div class="win">
  <nav>
    <button class:active={tab==="activity"} onclick={() => tab="activity"}>Activity</button>
    <button class:active={tab==="conflicts"} onclick={() => tab="conflicts"}>Conflicts{conflicts.length ? ` (${conflicts.length})` : ""}</button>
    <button class:active={tab==="notifications"} onclick={() => tab="notifications"}>Notifications{notifs.length ? ` (${notifs.length})` : ""}</button>
    <button class:active={tab==="blocked"} onclick={() => tab="blocked"}>Can't sync{realBlocked.length ? ` (${realBlocked.length})` : ""}</button>
    <button class:active={tab==="trash"} onclick={() => { tab="trash"; loadTrash(); }}>Trash</button>
  </nav>

  <div class="body">
    {#if tab === "activity"}
      {#if activity.length === 0}<p class="empty">Nothing yet.</p>{/if}
      {#each activity as e}
        <div class="line" class:hl={(e.path + NL + e.kind) === highlight}><span class="t">{e.time}</span>{#if e.account}<span class="acct">{e.account}</span>{/if}<span class="k">{e.kind}</span><span class="p">{e.path}</span>{#if e.err}<span class="err">⚠ {e.err}</span>{/if}</div>
      {/each}

    {:else if tab === "conflicts"}
      {#each otherAttn as o}
        <div class="otherattn">
          <span><b>{o.user}</b> has {o.conflicts ? `${o.conflicts} conflict${o.conflicts === 1 ? "" : "s"}` : ""}{o.conflicts && o.blocked ? " and " : ""}{o.blocked ? `${o.blocked} blocked file${o.blocked === 1 ? "" : "s"}` : ""} on its account.</span>
          <button class="link" onclick={() => showAccount(o.id)}>Show that account</button>
        </div>
      {/each}
      {#if conflicts.length === 0 && otherAttn.length === 0}<p class="empty">No conflicts. 🎉</p>{/if}
      {#each conflicts as c}
        <div class="card" class:hl={(c.localDir + NL + c.path) === highlight}>
          <div class="title">{c.path}</div>
          <div class="desc">{conflictDesc(c.kind)}</div>
          <div class="versions">
            <div class="ver" class:newest={newer(c) === "local"}>
              <div class="vhead">This computer{#if newer(c) === "local"} · newer{/if}</div>
              {#if c.localExists}
                <div class="vmeta">{humanBytes(c.localSize)} · {c.localMTime || "—"}</div>
              {:else}<div class="vmeta gone">deleted here</div>{/if}
            </div>
            <div class="ver" class:newest={newer(c) === "remote"}>
              <div class="vhead">Nextcloud{#if newer(c) === "remote"} · newer{/if}</div>
              {#if c.remoteExists}
                <div class="vmeta">{humanBytes(c.remoteSize)} · {c.remoteMTime || "—"}</div>
              {:else}<div class="vmeta gone">deleted on server</div>{/if}
            </div>
          </div>
          <button class="prevtoggle" onclick={() => togglePreview(c)}>
            {previewOpen[ckey(c)] ? "▾ Hide contents" : "▸ View contents"}
          </button>
          {#if previewOpen[ckey(c)]}
            {@const pv = previews[ckey(c)]}
            {#if !pv}
              <div class="pvloading">Loading…</div>
            {:else}
              <div class="previews">
                <div class="pvcol">
                  <div class="pvhead">This computer</div>
                  {#if pv.local.isText}
                    <pre class="pvtext">{pv.local.preview}</pre>
                    {#if pv.local.truncated}<div class="pvtrunc">… showing first 16 KB</div>{/if}
                  {:else}
                    <div class="pvnote">{pv.local.note || "—"}</div>
                  {/if}
                </div>
                <div class="pvcol">
                  <div class="pvhead">Nextcloud</div>
                  {#if pv.remote.isText}
                    <pre class="pvtext">{pv.remote.preview}</pre>
                    {#if pv.remote.truncated}<div class="pvtrunc">… showing first 16 KB</div>{/if}
                  {:else}
                    <div class="pvnote">{pv.remote.note || "—"}</div>
                  {/if}
                </div>
              </div>
            {/if}
          {/if}
          <div class="btns">
            <button class="primary" onclick={() => resolve(c, "local")}>Keep mine</button>
            <button onclick={() => resolve(c, "remote")}>Keep server</button>
            <button onclick={() => resolve(c, "both")}>Keep both</button>
          </div>
        </div>
      {/each}

    {:else if tab === "notifications"}
      {#if notifs.length === 0}<p class="empty">No notifications.</p>{/if}
      {#if notifs.length > 1}
        <div class="bulkrow">
          <span class="bulkcount">{notifs.length} notifications</span>
          <button onclick={dismissAllNotifs}>Dismiss all</button>
        </div>
      {/if}
      {#each notifs as n}
        <div class="card" class:hl={("notif" + NL + n.id) === highlight}>
          <div class="title">{n.subject || n.app}</div>
          {#if n.message}<div class="desc">{n.message}</div>{/if}
          <div class="btns">
            {#each n.actions as ac}<button onclick={() => App.DoNotificationAction(n.id, ac.label)}>{ac.label}</button>{/each}
            {#if n.link}<button onclick={() => App.OpenURL(n.link)}>Open</button>{/if}
            <button onclick={() => App.DismissNotification(n.id)}>Dismiss</button>
          </div>
        </div>
      {/each}

    {:else if tab === "blocked"}
      {#if blocked.length === 0}<p class="empty">Nothing blocked.</p>{/if}
      {#if realBlocked.length > 1}
        <div class="bulkrow">
          <span class="bulkcount">{realBlocked.length} files can't sync</span>
          <button class="danger" onclick={deleteAllBlocked}>Delete all</button>
        </div>
      {/if}
      {#each blocked as b}
        {#if b.escaping}
          <div class="card">
            <div class="title">Syncing {b.ext} files</div>
            <div class="desc">{b.reason}. Stored on the server under a “{b.ext}{'…'}” copy and shown here with their real name.</div>
            <div class="btns">
              <button onclick={() => stopEscape(b)}>Stop</button>
            </div>
          </div>
        {:else}
          <div class="card" class:hl={b.abs === highlight}>
            <div class="title">{b.path}</div>
            <div class="desc">{b.reason}</div>
            <div class="btns">
              {#if b.escapable}
                <button class="primary" onclick={() => syncEscape(b)}>Sync {b.ext} files (rename on server)</button>
              {/if}
              <button onclick={() => rename(b)}>Rename…</button>
              <button onclick={() => App.BlacklistBlocked(b.abs)}>Blacklist</button>
              <button class="danger" onclick={() => deleteBlocked(b)}>Delete</button>
            </div>
          </div>
        {/if}
      {/each}

    {:else}
      {#if trashBusy && trash.length === 0}<p class="empty">Loading…</p>
      {:else if trash.length === 0}<p class="empty">Trash is empty.</p>
      {:else if trash.length >= 500}<p class="empty">Showing the 500 most recently deleted.</p>{/if}
      {#each trash as t}
        <div class="trow" class:hl={t.href === highlight}>
          <div class="tmain">
            <div class="tname">{t.isDir ? "📁" : "📄"} {t.name}</div>
            <div class="tsub">{t.originalLocation || "—"}{#if t.deletedAt} · deleted {t.deletedAt}{/if}</div>
          </div>
          <div class="tbtns">
            <button class="primary" onclick={() => restoreTrash(t)}>Restore</button>
            <button onclick={() => deleteTrash(t)}>Delete</button>
          </div>
        </div>
      {/each}
    {/if}
  </div>
</div>

<style>
  .win { height: 100%; display: flex; flex-direction: column; background: var(--bg); color: var(--fg); }
  nav { display: flex; border-bottom: 1px solid var(--border); }
  nav button { flex: 1; padding: 12px 8px; border: none; background: none; cursor: pointer; font-size: 13px;
               color: var(--fg2); border-bottom: 2px solid transparent; }
  nav button:hover { background: var(--panel-2); }
  nav button.active { color: var(--accent); border-bottom-color: var(--accent); font-weight: 600; }
  .body { flex: 1; overflow-y: auto; padding: 12px 16px; }
  .line { display: flex; gap: 10px; padding: 5px 0; font-size: 13px; border-bottom: 1px solid var(--border-soft); }
  .line .t { color: var(--muted); font-variant-numeric: tabular-nums; }
  .line .acct { color: var(--accent); font-size: 11px; border: 1px solid var(--border); border-radius: 8px;
                padding: 0 6px; align-self: center; white-space: nowrap; }
  .otherattn { display: flex; justify-content: space-between; align-items: center; gap: 10px; font-size: 13px;
               background: var(--panel); border: 1px solid var(--border); border-radius: 8px; padding: 8px 10px; margin-bottom: 8px; }
  .line .k { color: var(--accent); min-width: 86px; }
  .line .p { flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .line .err { color: #e06b6b; }
  /* Deep-link flash: briefly tint + outline the row the flyout pointed at. */
  .hl { animation: hlflash 3.4s ease-out both; border-radius: 6px; }
  @keyframes hlflash {
    0%, 55% { background: var(--tint); box-shadow: inset 0 0 0 1.5px var(--accent); }
    100% { background: transparent; box-shadow: inset 0 0 0 0 transparent; }
  }
  .card { border: 1px solid var(--border); border-radius: 8px; padding: 12px; margin-bottom: 10px; }
  .card .title { font-weight: 600; font-size: 13px; }
  .card .desc { color: var(--fg2); font-size: 12px; margin: 4px 0 10px; }
  .versions { display: grid; grid-template-columns: 1fr 1fr; gap: 8px; margin: 0 0 12px; }
  .ver { border: 1px solid var(--border); border-radius: 7px; padding: 8px 10px; background: var(--panel); }
  .ver.newest { border-color: var(--accent); background: var(--tint); }
  .vhead { font-size: 11px; text-transform: uppercase; letter-spacing: .3px; color: var(--muted); font-weight: 600; }
  .ver.newest .vhead { color: var(--accent); }
  .vmeta { font-size: 12.5px; color: var(--fg); margin-top: 3px; }
  .vmeta.gone { color: #e06b6b; }
  .prevtoggle { background: none; border: none; color: var(--accent); cursor: pointer; font-size: 12px;
                padding: 0 0 8px; }
  .prevtoggle:hover { text-decoration: underline; }
  .pvloading { color: var(--muted); font-size: 12px; padding: 0 0 10px; }
  .previews { display: grid; grid-template-columns: 1fr 1fr; gap: 8px; margin: 0 0 12px; }
  .pvcol { min-width: 0; }
  .pvhead { font-size: 11px; text-transform: uppercase; letter-spacing: .3px; color: var(--muted); font-weight: 600; margin-bottom: 3px; }
  .pvtext { margin: 0; padding: 8px; max-height: 220px; overflow: auto; background: var(--panel);
            border: 1px solid var(--border); border-radius: 6px; font-size: 11.5px; line-height: 1.4;
            white-space: pre-wrap; word-break: break-word; font-family: ui-monospace, "Cascadia Code", Consolas, monospace; }
  .pvtrunc { font-size: 11px; color: var(--muted); margin-top: 3px; }
  .pvnote { font-size: 12px; color: var(--muted); font-style: italic; padding: 8px; border: 1px dashed var(--border); border-radius: 6px; }
  .btns { display: flex; gap: 8px; flex-wrap: wrap; }
  .btns button { padding: 6px 12px; border: 1px solid var(--border-2); border-radius: 6px; background: var(--panel-2);
                 color: var(--fg); cursor: pointer; font-size: 12px; }
  .btns button:hover { background: var(--hover); }
  .btns button.primary { background: var(--accent); border-color: var(--accent); color: #fff; }
  .btns button.danger { color: #c0392b; border-color: #e6b8b2; }
  .btns button.danger:hover { background: #fdeceb; }
  .bulkrow { display: flex; align-items: center; justify-content: space-between; gap: 10px;
             padding: 8px 10px; margin-bottom: 10px; border: 1px solid var(--border); border-radius: 8px; background: var(--panel); }
  .bulkcount { font-size: 12.5px; color: var(--fg2); }
  .bulkrow button { padding: 6px 12px; border: 1px solid var(--border-2); border-radius: 6px;
                    background: var(--panel-2); color: var(--fg); cursor: pointer; font-size: 12px; }
  .bulkrow button:hover { background: var(--hover); }
  .bulkrow button.danger { padding: 6px 12px; border: 1px solid #e6b8b2; border-radius: 6px;
                           background: var(--panel-2); color: #c0392b; cursor: pointer; font-size: 12px; }
  .bulkrow button.danger:hover { background: #fdeceb; }
  .empty { color: var(--muted); font-size: 13px; }
  .trow { display: flex; align-items: center; justify-content: space-between; gap: 10px; padding: 8px 0; border-bottom: 1px solid var(--border-soft); }
  .tmain { min-width: 0; }
  .tname { font-size: 13px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .tsub { font-size: 11.5px; color: var(--muted); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .tbtns { display: flex; gap: 8px; flex: 0 0 auto; }
  .tbtns button { padding: 5px 11px; border: 1px solid var(--border-2); border-radius: 6px; background: var(--panel-2); color: var(--fg); cursor: pointer; font-size: 12px; }
  .tbtns button:hover { background: var(--hover); }
  .tbtns button.primary { background: var(--accent); border-color: var(--accent); color: #fff; }
</style>
