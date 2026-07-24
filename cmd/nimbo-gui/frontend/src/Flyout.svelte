<script lang="ts">
  import { Events } from "@wailsio/runtime";
  import { App } from "../bindings/github.com/otherworld/nimbo/cmd/nimbo-gui";

  let brandName = $state("Nimbo");
  (async () => { brandName = (await App.Brand()).name || "Nimbo"; })();

  type AppInfo = { id: string; name: string; href: string; icon: string; pinned: boolean };

  type Header = {
    user: string; server: string; statusType: string; statusMsg: string; statusIcon: string;
    quotaUsed: number; quotaTotal: number; quotaPct: number; unlimited: boolean;
  };

  type Activity = { time: string; kind: string; path: string; remotePath: string; err: string };
  type Attention = { conflicts: number; blocked: number };
  type Progress = { active: boolean; current: string; done: number; total: number; speed: number; avgSpeed: number; doneBytes: number; totalBytes: number; enumerating: boolean };

  function etaText(p: Progress): string {
    // ETA off the cumulative average rate, not the jumpy instantaneous speed.
    if (!p.avgSpeed || p.totalBytes <= 0) return "";
    const remain = p.totalBytes - p.doneBytes;
    if (remain <= 0) return "";
    const s = remain / p.avgSpeed;
    if (s < 90) return `~${Math.max(1, Math.ceil(s))}s left`;
    if (s < 5400) return `~${Math.round(s / 60)}m left`;
    return `~${(s / 3600).toFixed(1)}h left`;
  }
  const syncPct = (p: Progress) =>
    p.totalBytes > 0 ? Math.round((p.doneBytes / p.totalBytes) * 100)
    : p.total > 0 ? Math.round((p.done / p.total) * 100) : 0;

  let status = $state("Starting…");
  let paused = $state(false);
  let progress = $state<Progress>({ active: false, current: "", done: 0, total: 0, speed: 0, avgSpeed: 0, doneBytes: 0, totalBytes: 0, enumerating: false });
  let recent = $state<Activity[]>([]);
  let apps = $state<AppInfo[]>([]);
  let editApps = $state(false);
  let showDock = $state(true);
  let dockSide = $state("right"); // "right" | "left" | "bottom"
  let showSearch = $state(true);
  let notifCount = $state(0);
  let header = $state<Header>({ user: "", server: "", statusType: "", statusMsg: "", statusIcon: "", quotaUsed: 0, quotaTotal: 0, quotaPct: 0, unlimited: false });
  let attention = $state<Attention>({ conflicts: 0, blocked: 0 });
  let pauseInfo = $state<{ paused: boolean; reason: string; until: string }>({ paused: false, reason: "", until: "" });
  let pauseMenu = $state(false);
  let editStatus = $state(false);
  let moreMenu = $state(false);
  let msgInput = $state("");

  // Side-by-side accounts: list them in the ⋯ menu with live status; choosing
  // one makes it the shown account (everything keeps syncing regardless).
  type AcctEntry = { id: string; user: string; server: string; active: boolean; status: string };
  let accounts = $state<AcctEntry[]>([]);
  let acctBusy = $state(false);
  const hostOf = (s: string) => { try { return new URL(s).host; } catch { return s; } };
  async function loadAccounts() { accounts = (await App.ListAccounts()) ?? []; }
  async function switchAccount(id: string) {
    acctBusy = true;
    const err = await App.SwitchAccount(id);
    acctBusy = false;
    if (err) { alert(err); return; }
    await refresh();
  }

  type SearchItem = { title: string; subline: string; href: string };
  let searchTerm = $state("");
  let searchResults = $state<SearchItem[]>([]);
  let searchOpen = $state(false);
  let searchBusy = $state(false);
  let searchTimer: ReturnType<typeof setTimeout>;
  let searchSeq = 0;

  // Flyout appearance (icon size / panel width / density / visible sections).
  let appearance = $state<{ dockIconSize: string; panelWidth: string; density: string; sections: string[] }>(
    { dockIconSize: "medium", panelWidth: "standard", density: "comfortable", sections: ["search", "activity", "storage"] });

  async function refresh() {
    status = await App.Status();
    pauseInfo = await App.PauseInfo();
    paused = pauseInfo.paused;
    progress = await App.Progress();
    recent = (await App.RecentActivity()) ?? [];
    const a = (await App.Apps()) ?? [];
    if (a.length || !apps.length) apps = a; // don't blank a populated rail on a transient empty fetch
    const h = await App.Header();
    if (h.user || !header.user) header = h; // don't flap the identity to "logged out"
    attention = await App.Attention();
    notifCount = await App.NotificationCount();
    showDock = await App.ShowAppDock();
    dockSide = await App.AppDockSide();
    showSearch = await App.ShowSearch();
    appearance = await App.FlyoutAppearance();
    msgInput = header.statusMsg;
  }

  function humanBytes(n: number): string {
    if (!n || n < 0) return "0 B";
    const u = ["B", "KB", "MB", "GB", "TB", "PB"];
    let i = 0;
    while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
    return `${n < 10 && i > 0 ? n.toFixed(1) : Math.round(n)} ${u[i]}`;
  }
  // Split bytes into number + unit so the storage strip can render them as
  // separate spans with a guaranteed gap (a plain space wasn't showing).
  function bytesParts(n: number): { n: string; u: string } {
    if (!n || n < 0) return { n: "0", u: "B" };
    const u = ["B", "KB", "MB", "GB", "TB", "PB"];
    let i = 0;
    while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
    return { n: n < 10 && i > 0 ? n.toFixed(1) : String(Math.round(n)), u: u[i] };
  }

  let attentionTotal = $derived(attention.conflicts + attention.blocked);
  let attentionText = $derived(
    [attention.conflicts && `${attention.conflicts} conflict${attention.conflicts > 1 ? "s" : ""}`,
     attention.blocked && `${attention.blocked} can’t sync`].filter(Boolean).join(" · ")
  );

  const basename = (p: string) => p.replace(/\/+$/, "").split("/").pop() || p;
  const kindIcon = (k: string) =>
    ({ download: "↓", upload: "↑", "delete-local": "🗑", "delete-remote": "🗑",
       "move-local": "↪", "move-remote": "↪", "mkdir-local": "📁", "mkdir-remote": "📁",
       conflict: "⚠" } as Record<string, string>)[k] ?? "•";
  const kindLabel = (k: string) =>
    ({ download: "Downloaded", upload: "Uploaded", "delete-local": "Deleted locally",
       "delete-remote": "Deleted on server", "move-local": "Moved", "move-remote": "Moved",
       "mkdir-local": "New folder", "mkdir-remote": "New folder", conflict: "Conflict" } as Record<string, string>)[k] ?? k;
  refresh();
  Events.On("status", (e: any) => {
    status = e.data;
    // The engine starts asynchronously; the first refresh() at mount may run
    // before it's ready (empty header/folders/apps). Re-fetch once it comes up.
    if (!header.user) refresh();
  });
  Events.On("progress", (e: any) => { progress = e.data; });
  // Activity fires per file — hundreds/sec during a big sync. Coalesce into a
  // light refresh (just the recent list) at most ~2×/sec, so the panel doesn't
  // thrash re-fetching/re-rendering the whole header + app rail on every file.
  let actPending = false;
  function refreshActivitySoon() {
    if (actPending) return;
    actPending = true;
    setTimeout(async () => {
      actPending = false;
      recent = (await App.RecentActivity()) ?? [];
    }, 500);
  }
  Events.On("activity", refreshActivitySoon);
  Events.On("appdock", (e: any) => { showDock = e.data; if (!showDock) editApps = false; });
  Events.On("appdock-side", (e: any) => { dockSide = e.data || "right"; });
  Events.On("searchbar", (e: any) => { showSearch = e.data; if (!showSearch) clearSearch(); });
  Events.On("appearance", async () => { appearance = await App.FlyoutAppearance(); });
  Events.On("notifications", async () => { notifCount = await App.NotificationCount(); });
  Events.On("conflicts", async () => { attention = await App.Attention(); });
  Events.On("blocked", async () => { attention = await App.Attention(); });

  const presenceLabel = (s: string) =>
    s === "online" ? "Online" : s === "away" ? "Away" : s === "dnd" ? "Do not disturb"
    : s === "invisible" ? "Invisible" : "Offline";

  async function setType(t: string) { await App.SetStatusType(t); await refresh(); }
  async function setMsg() { await App.SetStatusMessage(msgInput.trim()); await refresh(); }
  async function clearMsg() { await App.ClearStatusMessage(); msgInput = ""; await refresh(); }

  // Dock apps open as desktop windows (own window + taskbar identity); the
  // nimbo-app:// prefix routes inside the existing OpenURL binding (no regen).
  // Right-click is the escape hatch: plain href → default browser.
  const openApp = (a: AppInfo) => App.OpenURL("nimbo-app://" + a.id);
  const openAppInBrowser = (a: AppInfo) => App.OpenURL(a.href);
  const toggleShortcut = async (a: AppInfo) => {
    await App.OpenURL("nimbo-app-shortcut://" + a.id);
    apps = (await App.Apps()) ?? [];
  };
  const hasShortcut = (a: AppInfo) => (a as any).shortcut === true;
  // Clicking a recent-activity row opens the Sync status window on the Activity
  // tab and flashes that row. The target is newline-joined into OpenStatusTab's
  // single string arg as "<tab>\n<path>\n<kind>" (a newline never occurs in a tab
  // name or a file path), so this needs no new Go binding. Status splits it back.
  const openActivity = (r: Activity) =>
    App.OpenStatusTab("activity\n" + r.path + "\n" + r.kind);
  const pauseFor = (m: number) => { App.PauseFor(m); pauseMenu = false; setTimeout(refresh, 150); };
  const untilTomorrow = () => { App.PauseUntilTomorrow(); pauseMenu = false; setTimeout(refresh, 150); };
  const resume = () => { App.Resume(); setTimeout(refresh, 150); };

  async function togglePin(a: AppInfo) {
    if (a.pinned) { await App.UnpinApp(a.id); } else { await App.PinApp(a.id); }
    apps = (await App.Apps()) ?? [];
  }

  function onSearchInput() {
    clearTimeout(searchTimer);
    const q = searchTerm.trim();
    if (q.length < 2) { searchResults = []; searchOpen = false; searchBusy = false; return; }
    searchBusy = true;
    const seq = ++searchSeq;
    searchTimer = setTimeout(async () => {
      const r = (await App.Search(q)) ?? [];
      if (seq !== searchSeq) return; // a newer keystroke superseded this
      searchResults = r;
      searchOpen = true;
      searchBusy = false;
    }, 250);
  }
  function clearSearch() {
    clearTimeout(searchTimer);
    searchSeq++;
    searchTerm = ""; searchResults = []; searchOpen = false; searchBusy = false;
  }
  function openResult(item: SearchItem) {
    if (item.href) App.OpenURL(item.href);
    clearSearch();
  }

  let pinned = $derived(apps.filter(a => a.pinned));
  let dotClass = $derived(
    paused ? "dot paused"
    : /sync|scan/.test(status.toLowerCase()) ? "dot syncing"
    : "dot ok"
  );
</script>

<div class="panel" class:dock-left={showDock && dockSide === "left"} class:dock-bottom={showDock && dockSide === "bottom"}
     class:dense={appearance.density === "compact"} class:icons-sm={appearance.dockIconSize === "small"} class:icons-lg={appearance.dockIconSize === "large"}>
  <div class="main">
  <header>
    <div class="idrow">
      {#if header.user}
        <button class="user" onclick={() => (editStatus = !editStatus)} title="Set status">
          <span class="presence {header.statusType || 'offline'}"></span>
          <span class="idtext">
            <span class="uname">{header.user}</span>
            <span class="ustatus">{header.statusIcon} {header.statusMsg || presenceLabel(header.statusType)}</span>
          </span>
          <span class="caret">{editStatus ? "▴" : "▾"}</span>
        </button>
      {:else}
        <div class="brand">{brandName}</div>
      {/if}
      {#if header.user}
        <div class="tools">
          <button class="tool notif" onclick={() => App.OpenStatusTab("notifications")}
                  title={notifCount > 0 ? `${notifCount} notification${notifCount === 1 ? "" : "s"}` : "Notifications"}>
            🔔
            {#if notifCount > 0}<span class="badge">{notifCount > 99 ? "99+" : notifCount}</span>{/if}
          </button>
          <button class="tool" onclick={() => App.OpenSyncFolder()} title="Open your {brandName} folder">📁</button>
          {#if pauseInfo.paused}
            <button class="tool on" onclick={resume} title={pauseInfo.until ? `Paused until ${pauseInfo.until} — resume` : "Resume syncing"}>▶</button>
          {:else}
            <button class="tool" class:on={pauseMenu} onclick={() => { pauseMenu = !pauseMenu; moreMenu = false; }} title="Pause syncing">❚❚</button>
          {/if}
          <button class="tool" class:on={moreMenu} onclick={() => { moreMenu = !moreMenu; pauseMenu = false; if (moreMenu) loadAccounts(); }} title="More">⋯</button>
        </div>
      {/if}
    </div>

    {#if pauseMenu && !pauseInfo.paused}
      <div class="pausemenu">
        <button onclick={() => pauseFor(60)}>1 hour</button>
        <button onclick={() => pauseFor(240)}>4 hours</button>
        <button onclick={untilTomorrow}>Until tomorrow</button>
        <button onclick={() => pauseFor(0)}>Indefinitely</button>
      </div>
    {/if}

    {#if moreMenu}
      <div class="pausemenu moremenu">
        {#if accounts.length > 1}
          {#each accounts as ac}
            <button class="acctitem" class:cur={ac.active} disabled={acctBusy}
                    title={ac.active ? "Shown account" : "Show this account"}
                    onclick={() => { if (!ac.active) { moreMenu = false; switchAccount(ac.id); } }}>
              <span class="acctmain">{ac.active ? "✓ " : ""}{ac.user} <span class="accthost">· {hostOf(ac.server)}</span></span>
              {#if ac.status}<span class="acctstat">{ac.status}</span>{/if}
            </button>
          {/each}
        {/if}
        <button onclick={() => { moreMenu = false; App.OpenSettings(); }}>Settings</button>
        <button onclick={() => { moreMenu = false; App.OpenStatus(); }}>Sync status</button>
        <button onclick={() => { moreMenu = false; App.SyncNow(); }}>Sync now</button>
        <button class="quit" onclick={() => App.Quit()}>Quit {brandName}</button>
      </div>
    {/if}

    {#if header.user && editStatus}
      <div class="statusedit">
        <div class="types">
          <button class:sel={header.statusType==="online"} onclick={() => setType("online")}><span class="presence online"></span>Online</button>
          <button class:sel={header.statusType==="away"} onclick={() => setType("away")}><span class="presence away"></span>Away</button>
          <button class:sel={header.statusType==="dnd"} onclick={() => setType("dnd")}><span class="presence dnd"></span>Busy</button>
          <button class:sel={header.statusType==="invisible"} onclick={() => setType("invisible")}><span class="presence invisible"></span>Invisible</button>
        </div>
        <div class="msgrow">
          <input placeholder="What is your status?" bind:value={msgInput}
                 onkeydown={(e) => e.key === "Enter" && setMsg()} />
          <button onclick={setMsg}>Set</button>
          <button onclick={clearMsg}>Clear</button>
        </div>
      </div>
    {/if}
  </header>

  {#if attentionTotal > 0}
    <button class="alert" onclick={() => App.OpenStatusTab(attention.conflicts > 0 ? "conflicts" : "blocked")}>
      <span class="warn">⚠</span>
      <span class="atext">{attentionText} need{attentionTotal === 1 ? "s" : ""} attention</span>
      <span class="go">Review →</span>
    </button>
  {/if}

  <!-- The three flyout sections render in the order the user set (Settings →
       Appearance); hidden ones are simply absent from appearance.sections. -->
  {#snippet searchSection()}
    {#if header.user}
    <div class="search" class:open={searchOpen && searchResults.length > 0}>
      <div class="searchbar">
        <span class="sicon">🔍</span>
        <input
          type="text"
          placeholder="Search your files…"
          bind:value={searchTerm}
          oninput={onSearchInput}
          onkeydown={(e) => e.key === "Escape" && clearSearch()} />
        {#if searchBusy}
          <span class="spin"></span>
        {:else if searchTerm}
          <button class="sclear" onclick={clearSearch} title="Clear">✕</button>
        {/if}
      </div>
      {#if searchOpen}
        <div class="sresults">
          {#if searchResults.length === 0}
            <p class="empty">No files match “{searchTerm.trim()}”.</p>
          {:else}
            {#each searchResults as r}
              <button class="sresult" onclick={() => openResult(r)} title={r.subline}>
                <span class="rtitle">{r.title}</span>
                {#if r.subline}<span class="rsub">{r.subline}</span>{/if}
              </button>
            {/each}
          {/if}
        </div>
      {/if}
    </div>
    {/if}
  {/snippet}

  {#snippet activitySection()}
  <div class="acthead">
    <div class="acttop">
      <h2>Recent activity</h2>
      <div class="syncstat" title={progress.active && !paused ? progress.current : ""}>
        {#if progress.active && !paused}
          <span class="dot syncing"></span>
          {#if progress.enumerating}
            <span class="sstext">Scanning…</span>
          {:else}
            <span class="sstext">Syncing {syncPct(progress)}%</span>
          {/if}
        {:else}
          <span class={dotClass}></span>
          <span class="sstext">{pauseInfo.paused ? (pauseInfo.until ? "Paused · " + pauseInfo.until : "Paused") : status}</span>
        {/if}
      </div>
    </div>
    {#if progress.active && !paused}
      {#if progress.enumerating}
        <!-- Total not final yet — show an indeterminate bar, not a misleading % -->
        <div class="syncbar"><div class="syncfill indet"></div></div>
        <div class="syncmeta">
          {progress.done.toLocaleString()} downloaded{#if progress.speed > 0} · {humanBytes(progress.speed)}/s{/if} · still counting…
        </div>
      {:else}
        {@const eta = etaText(progress)}
        <div class="syncbar"><div class="syncfill" style="width:{syncPct(progress)}%"></div></div>
        {#if progress.total > 0}
          <div class="syncmeta">
            {progress.done.toLocaleString()} / {progress.total.toLocaleString()}{#if progress.speed > 0} · {humanBytes(progress.speed)}/s{/if}{#if eta} · {eta}{/if}
          </div>
        {/if}
      {/if}
      {#if progress.current}
        <div class="synccur" title={progress.current}>
          <span class="curspin"></span>
          <span class="curfile">{basename(progress.current)}</span>
        </div>
      {/if}
    {/if}
  </div>

  <section class="scroll">
    {#if recent.length === 0}
      <p class="empty">Nothing synced recently.</p>
    {:else}
      <div class="activity">
        {#each recent.slice(0, 6) as r}
          <button class="act" class:err={r.err} onclick={() => openActivity(r)}
                  title={`Open in Sync status · ${kindLabel(r.kind)} · ${r.path}${r.err ? " · " + r.err : ""}`}>
            <span class="aicon {r.kind}">{kindIcon(r.kind)}</span>
            <span class="abody">
              <span class="apath">{basename(r.path)}</span>
              <span class="akind">{kindLabel(r.kind)}</span>
            </span>
            <span class="atime">{r.err ? "failed" : r.time}</span>
          </button>
        {/each}
      </div>
    {/if}
  </section>
  {/snippet}

  {#snippet storageSection()}
    {#if header.user && header.quotaUsed > 0}
    {@const metered = !header.unlimited && header.quotaTotal > 0}
    {@const used = bytesParts(header.quotaUsed)}
    <div class="storage" title="Storage{header.server ? ' on ' + header.server.replace(/^https?:\/\//, '') : ''}">
      <span class="stxt">
        <span class="snum">{used.n}</span><span class="sunit">{used.u}</span>{#if metered}{@const tot = bytesParts(header.quotaTotal)}<span class="sof">of</span><span class="snum">{tot.n}</span><span class="sunit">{tot.u}</span>{:else}<span class="sof">used</span>{/if}
      </span>
      {#if metered}
        <div class="sbar"><div class="sfill" class:full={header.quotaPct >= 90} style="width:{Math.min(header.quotaPct, 100)}%"></div></div>
        <span class="spct" class:full={header.quotaPct >= 90}>{header.quotaPct.toFixed(0)}%</span>
      {/if}
    </div>
    {/if}
  {/snippet}

  {#each appearance.sections as key (key)}
    {#if key === "search"}{@render searchSection()}
    {:else if key === "activity"}{@render activitySection()}
    {:else if key === "storage"}{@render storageSection()}
    {/if}
  {/each}

  {#if editApps}
    <div class="appmanage">
      <div class="amhead">
        <h2>Pin apps to the dock</h2>
        <button class="link" onclick={() => (editApps = false)}>Done</button>
      </div>
      {#if apps.length === 0}
        <p class="empty">No apps available yet.</p>
      {:else}
        <div class="amlist">
          {#each apps as a}
            <div class="amrow" class:pinned={a.pinned}>
              <button class="amhit" onclick={() => togglePin(a)} title={a.pinned ? "Unpin from dock" : "Pin to dock"}>
                <span class="ic">{#if a.icon}<img src={a.icon} alt="" />{:else}🗂{/if}</span>
                <span class="amname">{a.name}</span>
                <span class="ampin">{a.pinned ? "★" : "☆"}</span>
              </button>
              <button class="amstart" class:on={hasShortcut(a)} onclick={() => toggleShortcut(a)}
                      title={hasShortcut(a) ? "Remove from the Start menu" : "Add to the Start menu (launch or pin it like a desktop app)"}>
                {hasShortcut(a) ? "✓" : "+"}<span class="amstartlbl">Start</span>
              </button>
            </div>
          {/each}
        </div>
      {/if}
    </div>
  {/if}
  </div>

  {#if header.user && showDock}
    <aside class="rail">
      <div class="raillist">
        {#each pinned as a}
          <button class="railapp" onclick={() => openApp(a)}
                  oncontextmenu={(e) => { e.preventDefault(); openAppInBrowser(a); }}
                  title={`${a.name} — opens in its own window (right-click for browser)`}>
            {#if a.icon}<img src={a.icon} alt={a.name} />{:else}<span class="railglyph">🗂</span>{/if}
          </button>
        {/each}
      </div>
      <button class="railedit" class:on={editApps} onclick={() => (editApps = !editApps)}
              title={pinned.length ? "Pin or unpin apps" : "Pin apps to the dock"}>✎</button>
    </aside>
  {/if}
</div>

<style>
  .panel { background: var(--bg); color: var(--fg); height: 100%; display: flex; flex-direction: row; overflow: hidden; }
  .panel.dock-left { flex-direction: row-reverse; }   /* rail on the left edge */
  .panel.dock-bottom { flex-direction: column; }       /* rail as a bottom strip */
  .main { flex: 1; min-width: 0; display: flex; flex-direction: column; position: relative; }
  header { padding: 12px 16px; border-bottom: 1px solid var(--border); }
  .idrow { display: flex; align-items: center; gap: 10px; }
  .brand { font-weight: 700; font-size: 16px; color: var(--fg); flex: 1; }
  .user { flex: 1; min-width: 0; display: flex; align-items: center; gap: 9px; background: none; border: none;
          padding: 3px; margin: -3px; border-radius: 8px; cursor: pointer; text-align: left; }
  .user:hover { background: var(--hover); }
  .user:hover .uname { color: var(--accent); }
  .idtext { flex: 1; min-width: 0; display: flex; flex-direction: column; }
  .uname { font-weight: 700; font-size: 16px; color: var(--fg); text-transform: capitalize; line-height: 1.25;
           overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .ustatus { color: var(--fg2); font-size: 11.5px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .caret { color: var(--muted); font-size: 11px; flex: 0 0 auto; }
  .presence { width: 10px; height: 10px; border-radius: 50%; display: inline-block; flex: 0 0 auto; }
  .tools { flex: 0 0 auto; display: flex; gap: 5px; }
  .tool { width: 32px; height: 32px; display: inline-flex; align-items: center; justify-content: center;
          border: 1px solid var(--border-2); border-radius: 8px; background: var(--panel-2); color: var(--fg2);
          cursor: pointer; font-size: 14px; line-height: 1; }
  .tool:hover, .tool.on { background: var(--tint); border-color: var(--accent); color: var(--accent); }
  .tool.notif { position: relative; }
  .badge { position: absolute; top: -4px; right: -4px; min-width: 16px; height: 16px; padding: 0 3px;
           border-radius: 8px; background: #d63939; color: #fff; font-size: 10px; font-weight: 700;
           line-height: 16px; text-align: center; box-sizing: border-box; }
  /* Primary action (Sync now) is filled with the user's Nextcloud theme colour. */
  .tool.primary { background: var(--accent); border-color: var(--accent); color: #fff; }
  .tool.primary:hover { background: var(--accent-dark); border-color: var(--accent-dark); color: #fff; }
  .presence.online { background: #2fb344; }
  .presence.away { background: #f0a020; }
  .presence.dnd { background: #d63939; }
  .presence.invisible, .presence.offline { background: #b0b0b0; }
  .statusedit { margin-top: 8px; padding: 8px; background: var(--panel); border: 1px solid var(--border); border-radius: 8px; }
  .types { display: grid; grid-template-columns: 1fr 1fr; gap: 4px; margin-bottom: 6px; }
  .types button { display: flex; align-items: center; gap: 6px; padding: 6px 8px; border: 1px solid transparent;
                  background: none; border-radius: 6px; cursor: pointer; font-size: 12px; color: var(--fg); }
  .types button:hover { background: var(--hover); }
  .types button.sel { background: var(--tint); border-color: var(--accent); font-weight: 600; }
  .msgrow { display: flex; gap: 4px; }
  .msgrow input { flex: 1; min-width: 0; padding: 5px 7px; border: 1px solid var(--border-2); border-radius: 5px; font-size: 12px; background: var(--bg); color: var(--fg); }
  .msgrow button { padding: 5px 8px; border: 1px solid var(--border-2); border-radius: 5px; background: var(--bg); color: var(--fg); cursor: pointer; font-size: 12px; }
  .msgrow button:hover { background: var(--hover); }
  .syncbar { height: 5px; border-radius: 3px; background: var(--border); overflow: hidden; }
  .syncfill { height: 100%; background: var(--accent); border-radius: 3px; transition: width .25s; }
  .syncfill.indet { width: 35%; transition: none; animation: indet 1.3s ease-in-out infinite; }
  @keyframes indet { 0% { margin-left: -35%; } 100% { margin-left: 100%; } }
  /* Compact storage strip above the footer (was a 2-line block in the header). */
  .storage { display: flex; align-items: center; gap: 9px; padding: 7px 16px; border-top: 1px solid var(--border-soft); }
  .stxt { flex: 0 0 auto; font-size: 11px; color: var(--muted); white-space: nowrap; }
  .stxt .sunit { margin-left: 3px; }   /* gap between number and its unit */
  .stxt .sof { margin: 0 4px; }        /* spacing around "of" / "used" */
  .sbar { flex: 1; min-width: 24px; height: 4px; border-radius: 2px; background: var(--border); overflow: hidden; }
  .sfill { height: 100%; background: var(--accent); border-radius: 2px; transition: width .3s; }
  .sfill.full { background: #d63939; }
  .spct { flex: 0 0 auto; font-size: 11px; font-weight: 600; color: var(--muted); }
  .spct.full { color: #d63939; }
  .alert { display: flex; align-items: center; gap: 8px; width: 100%; padding: 9px 16px; border: none;
           border-bottom: 1px solid #f5e3b0; background: #fff8e6; color: #8a6d1a; cursor: pointer; font-size: 12.5px; text-align: left; }
  .alert:hover { background: #fdf2d4; }
  .alert .warn { font-size: 14px; }
  .alert .atext { flex: 1; font-weight: 500; }
  .alert .go { color: #8a6d1a; font-weight: 600; white-space: nowrap; }
  .dot { width: 9px; height: 9px; border-radius: 50%; display: inline-block; }
  .dot.ok { background: #2e7d32; } .dot.syncing { background: var(--accent); } .dot.paused { background: var(--muted); }
  .pausemenu { display: flex; flex-wrap: wrap; gap: 6px; margin-top: 10px; }
  .pausemenu button { flex: 1 1 auto; padding: 6px 10px; border: 1px solid var(--border); border-radius: 6px;
                      background: var(--bg); color: var(--fg); cursor: pointer; font-size: 12px; white-space: nowrap; }
  .pausemenu button:hover { background: var(--tint); border-color: var(--accent); }
  .moremenu button { flex: 1 1 45%; }
  .moremenu .quit { color: #c0392b; }
  .moremenu .quit:hover { background: #fdeceb; border-color: #f0c0bb; }
  /* Account rows in the ⋯ menu: full-width, name left + live status right. */
  .moremenu .acctitem { flex: 1 1 100%; display: flex; justify-content: space-between; align-items: baseline; gap: 8px; text-align: left; }
  .moremenu .acctitem.cur { font-weight: 600; cursor: default; }
  .moremenu .acctmain { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .moremenu .accthost { color: var(--muted, #888); font-weight: 400; }
  .moremenu .acctstat { color: var(--muted, #888); font-size: 11px; white-space: nowrap; max-width: 40%; overflow: hidden; text-overflow: ellipsis; }
  .search { padding: 8px 16px 4px; position: relative; }
  .searchbar { display: flex; align-items: center; gap: 7px; padding: 6px 9px; background: var(--panel-2);
               border: 1px solid var(--border-2); border-radius: 8px; }
  .searchbar:focus-within { border-color: var(--accent); }
  .search.open .searchbar { border-bottom-left-radius: 0; border-bottom-right-radius: 0; }
  .sicon { font-size: 12px; opacity: .65; }
  .searchbar input { flex: 1; border: none; outline: none; background: none; color: var(--fg); font-size: 13px; }
  .searchbar input::placeholder { color: var(--muted); }
  .sclear { border: none; background: none; color: var(--muted); cursor: pointer; font-size: 12px; padding: 0 2px; }
  .sclear:hover { color: var(--fg); }
  .spin { width: 12px; height: 12px; border: 2px solid var(--border); border-top-color: var(--accent);
          border-radius: 50%; animation: spin .7s linear infinite; }
  @keyframes spin { to { transform: rotate(360deg); } }
  .sresults { max-height: 232px; overflow-y: auto; border: 1px solid var(--accent); border-top: none;
              border-radius: 0 0 8px 8px; background: var(--bg); }
  .sresults .empty { padding: 10px 11px; }
  .sresult { display: flex; flex-direction: column; gap: 1px; width: 100%; padding: 7px 11px; border: none;
             background: none; color: var(--fg); cursor: pointer; text-align: left; }
  .sresult:hover { background: var(--tint); }
  .sresult .rtitle { font-size: 13px; font-weight: 500; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
  .sresult .rsub { font-size: 11px; color: var(--muted); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
  .acthead { flex: 0 0 auto; padding: 12px 16px 8px; }
  .acttop { display: flex; align-items: baseline; justify-content: space-between; gap: 10px; }
  .acthead h2 { margin: 0; }
  .acttop h2 { flex: 0 0 auto; white-space: nowrap; }   /* never wrap "Recent activity" */
  .acthead .syncbar { margin-top: 8px; }
  .syncmeta { margin-top: 4px; font-size: 11px; color: var(--muted); }
  /* The file currently in flight — names what's syncing right now (the count/speed line above doesn't). */
  .synccur { display: flex; align-items: center; gap: 6px; margin-top: 4px; min-width: 0; }
  .curspin { flex: 0 0 auto; width: 9px; height: 9px; border: 2px solid var(--border); border-top-color: var(--accent);
             border-radius: 50%; animation: spin .7s linear infinite; }
  .curfile { font-size: 11.5px; color: var(--fg2); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
  .syncstat { flex: 0 1 auto; display: flex; align-items: center; gap: 6px; min-width: 0; }
  .syncstat .sstext { font-size: 12px; color: var(--fg2); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
  .scroll { flex: 1; min-height: 0; padding: 0 16px 8px; overflow-y: auto; }
  /* Apps sit at the bottom and grow upward as more are pinned (capped, then scroll). */
  h2 { font-size: 12px; text-transform: uppercase; color: var(--muted); margin: 14px 0 8px; }
  .activity { display: flex; flex-direction: column; }
  .act { display: flex; align-items: center; gap: 10px; width: 100%; padding: 7px 4px; border: 0;
         border-bottom: 1px solid var(--border-soft); background: none; color: inherit; font: inherit;
         text-align: left; cursor: pointer; border-radius: 6px; }
  .act:hover { background: var(--hover); }
  .act:last-child { border-bottom: none; }
  .aicon { flex: 0 0 auto; width: 24px; height: 24px; border-radius: 50%; display: inline-flex;
           align-items: center; justify-content: center; font-size: 12px; background: var(--hover); color: var(--fg2); }
  .aicon.download { background: var(--tint); color: var(--accent); }
  .aicon.upload { background: #e8f5ea; color: #2e7d32; }
  .aicon.conflict { background: #fdeceb; color: #c0392b; }
  .abody { flex: 1; min-width: 0; display: flex; flex-direction: column; }
  .apath { font-size: 13px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .akind { font-size: 11px; color: var(--muted); }
  .atime { flex: 0 0 auto; font-size: 11px; color: var(--muted); }
  .act.err .apath { color: #e06b6b; }
  .act.err .atime { color: #e06b6b; }
  .ic { flex: 0 0 auto; width: 22px; height: 22px; border-radius: 6px; background: var(--accent);
        display: inline-flex; align-items: center; justify-content: center; font-size: 13px; }
  .ic img { width: 14px; height: 14px; filter: brightness(0) invert(1); }

  /* App dock — a 54px rail. Default is the right edge (full height); the panel
     direction modifiers move it to the left edge or a bottom strip. */
  .rail { flex: 0 0 54px; display: flex; flex-direction: column; align-items: center; gap: 8px;
          padding: 10px 0; border-left: 1px solid var(--border); background: var(--panel); }
  .raillist { flex: 1; min-height: 0; width: 100%; display: flex; flex-direction: column; align-items: center;
              gap: 8px; overflow-y: auto; }
  /* Left edge: divider flips to the inner (right) side. */
  .panel.dock-left .rail { border-left: none; border-right: 1px solid var(--border); }
  /* Bottom strip: the rail becomes a horizontal row of icons. */
  .panel.dock-bottom .rail { flex-direction: row; padding: 8px 10px;
                             border-left: none; border-top: 1px solid var(--border); }
  .panel.dock-bottom .raillist { flex-direction: row; width: auto; overflow-x: auto; overflow-y: hidden; }
  .railapp { flex: 0 0 auto; width: 38px; height: 38px; display: inline-flex; align-items: center; justify-content: center;
             border: none; border-radius: 10px; background: var(--accent); cursor: pointer; transition: background .15s; }
  .railapp:hover { background: var(--accent-dark); }
  .railapp img { width: 20px; height: 20px; filter: brightness(0) invert(1); }
  .railglyph { font-size: 18px; }
  .railedit { flex: 0 0 auto; width: 38px; height: 38px; display: inline-flex; align-items: center; justify-content: center;
              border: 1px solid var(--border-2); border-radius: 10px; background: var(--panel-2); color: var(--fg2);
              cursor: pointer; font-size: 15px; }
  .railedit:hover, .railedit.on { background: var(--tint); border-color: var(--accent); color: var(--accent); }

  /* "Pin apps" editor — overlays the main column */
  .appmanage { position: absolute; inset: 0; z-index: 5; background: var(--bg); display: flex; flex-direction: column;
               padding: 14px 16px; }
  .amhead { display: flex; align-items: center; justify-content: space-between; }
  .amhead h2 { margin: 0; }
  .amlist { flex: 1; min-height: 0; overflow-y: auto; display: flex; flex-direction: column; gap: 5px; margin-top: 8px; }
  .amrow { display: flex; align-items: center; gap: 6px; width: 100%; padding: 4px 6px 4px 4px; border: 1px solid var(--border);
           border-radius: 8px; background: var(--panel); color: var(--fg); }
  .amrow.pinned { border-color: var(--accent); background: var(--tint); }
  .amhit { flex: 1; min-width: 0; display: flex; align-items: center; gap: 10px; padding: 4px 5px; border: none;
           border-radius: 6px; background: none; color: inherit; cursor: pointer; text-align: left; }
  .amhit:hover { background: var(--hover); }
  .amstart { flex: 0 0 auto; display: inline-flex; align-items: center; gap: 3px; padding: 4px 8px; font-size: 11px;
             border: 1px solid var(--border-2); border-radius: 6px; background: var(--panel-2); color: var(--fg2); cursor: pointer; }
  .amstart:hover { background: var(--hover); }
  .amstart.on { border-color: var(--accent); color: var(--accent); background: var(--tint); }
  .amstartlbl { font-size: 10.5px; letter-spacing: .02em; }
  .amname { flex: 1; min-width: 0; font-size: 13px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .ampin { flex: 0 0 auto; font-size: 15px; color: var(--muted); }
  .amrow.pinned .ampin { color: var(--accent); }
  .link { background: none; border: none; color: var(--accent); cursor: pointer; font-size: 12px; padding: 2px 4px; }
  .link:hover { text-decoration: underline; }
  .empty { color: var(--muted); font-size: 13px; }

  /* Appearance customisation. Density "compact" tightens spacing/fonts; icon size
     scales the dock icons. Panel width is handled by resizing the window (Go). */
  .panel.dense header { padding: 8px 12px; }
  .panel.dense .search { padding: 6px 12px 3px; }
  .panel.dense .acthead { padding: 8px 12px 5px; }
  .panel.dense .scroll { padding: 0 12px 5px; }
  .panel.dense .act { padding: 4px 4px; }
  .panel.dense .apath { font-size: 12px; }
  .panel.dense .akind, .panel.dense .atime { font-size: 10px; }
  .panel.dense .storage { padding: 5px 12px; }
  .panel.icons-sm .railapp { width: 30px; height: 30px; }
  .panel.icons-sm .railapp img { width: 16px; height: 16px; }
  .panel.icons-lg .railapp { width: 44px; height: 44px; }
  .panel.icons-lg .railapp img { width: 24px; height: 24px; }
  /* The rail column (and its ✎ button) track the icon size, so the whole dock
     strip shrinks/grows with the icons instead of leaving dead space around them.
     The freed width reflows into the content column. */
  .panel.icons-sm .rail { flex-basis: 44px; }
  .panel.icons-sm .railedit { width: 30px; height: 30px; }
  .panel.icons-lg .rail { flex-basis: 60px; }
  .panel.icons-lg .railedit { width: 44px; height: 44px; }
  /* A little bottom breathing room so the last icon isn't flush-cut when the rail scrolls. */
  .raillist { padding-bottom: 2px; }
</style>
