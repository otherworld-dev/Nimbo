<script lang="ts">
  import { App } from "../bindings/github.com/otherworld/nimbo/cmd/nimbo-gui";

  // Called when onboarding finishes (parent closes the window).
  let { done }: { done: () => void } = $props();

  type Info = {
    user: string; server: string; defaultDir: string;
    accountBytes: number; freeBytes: number; onDemandSupport: boolean;
  };

  let info = $state<Info | null>(null);
  let localDir = $state("");
  let freeBytes = $state(0);
  let mode = $state("everything"); // "everything" | "choose" | "ondemand"
  let busy = $state(false);
  let err = $state("");

  (async () => {
    info = await App.GetSetupInfo();
    localDir = info.defaultDir;
    freeBytes = info.freeBytes;
    if (!info.onDemandSupport && mode === "ondemand") mode = "everything";
  })();

  function fmt(bytes: number): string {
    if (!bytes || bytes < 0) return "—";
    const u = ["B", "KB", "MB", "GB", "TB"];
    let n = bytes, i = 0;
    while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
    return `${n.toFixed(n < 10 && i > 0 ? 1 : 0)} ${u[i]}`;
  }

  async function chooseFolder() {
    const p = await App.PickLocalFolder(localDir);
    if (p) {
      localDir = p;
      freeBytes = await App.FreeSpace(p);
    }
  }

  // Adopt confirmation: choosing virtual files over a folder that already
  // holds files returns a scan summary (JSON) instead of switching straight
  // away — the user confirms before anything in the folder is touched. The
  // scan crawls the whole account (minutes on a big one), so it gets a
  // blocking overlay with a live folder count and a Cancel.
  let adopt = $state<any>(null);
  let scanning = $state(false);
  let scanDirs = $state(0);
  let scanCancelled = false;
  let scanPoll: ReturnType<typeof setInterval> | null = null;

  async function connect() {
    busy = true; err = "";
    if (mode === "ondemand") {
      scanning = true; scanCancelled = false; scanDirs = 0;
      scanPoll = setInterval(async () => {
        const d: any = await App.Diagnostics();
        scanDirs = d?.adoptScanDirs || 0;
      }, 1500);
    }
    // The folders to download first ride on the mode (no extra App method).
    const arg = mode === "everything" && first.length ? "everything:" + JSON.stringify(first) : mode;
    const msg = await App.CompleteSetup(localDir, arg);
    if (scanPoll) { clearInterval(scanPoll); scanPoll = null; }
    scanning = false;
    busy = false;
    if (scanCancelled) { scanCancelled = false; return; }
    if (msg) {
      if (mode === "ondemand" && msg.startsWith("{")) {
        try {
          const sum = JSON.parse(msg);
          if (sum.error) { err = sum.error; return; }
          adopt = sum; return;
        } catch {}
      }
      err = msg; return;
    }
    if (mode === "choose") await App.OpenSettings();
    done();
  }

  async function cancelScan() {
    scanCancelled = true;
    scanning = false;
    await App.SetSyncMode("ondemand-cancel"); // aborts the crawl in Go
  }

  async function confirmAdopt() {
    adopt = null; busy = true;
    const msg = await App.SetSyncMode("ondemand-adopt");
    busy = false;
    if (msg) { err = msg; return; }
    done();
  }

  async function cancelAdopt() {
    adopt = null;
    await App.SetSyncMode("ondemand-cancel"); // drop the held plan; nothing was changed
  }

  function skip() { done(); }

  // "Download these first" (Deck #766): with everything syncing, the first
  // download of a big account takes hours. The folders ticked here come down
  // before the rest. Optional; the order they are ticked in is kept.
  type Entry = { name: string; path: string; isDir: boolean };
  let first = $state<string[]>([]);
  let picking = $state(false);
  let pcur = $state("");
  let pentries = $state<Entry[]>([]);
  let ploading = $state(false);
  async function pickGo(path: string) {
    pcur = path.replace(/^\/+|\/+$/g, "");
    ploading = true;
    pentries = (((await App.BrowseRemote(pcur)) ?? []) as Entry[]).filter(e => e.isDir);
    ploading = false;
  }
  function openPick() { picking = true; pickGo(""); }
  function pickUp() { if (pcur) pickGo(pcur.split("/").slice(0, -1).join("/")); }
  function toggleFirst(path: string) {
    first = first.includes(path) ? first.filter(x => x !== path) : [...first, path];
  }
  const firstSummary = $derived(first.length <= 3 ? first.join(", ") : `${first.slice(0, 3).join(", ")} and ${first.length - 3} more`);
</script>

<div class="setup">
  {#if info}
    <div class="head">
      <div class="who">
        <div class="avatar">{(info.user || "?").charAt(0).toUpperCase()}</div>
        <div class="name">{info.user}</div>
        <div class="sub">{info.server.replace(/^https?:\/\//, "")}</div>
      </div>
      <div class="arrow">↔</div>
      <div class="who">
        <div class="folder">📁</div>
        <div class="name">Local Folder</div>
        <div class="sub path" title={localDir}>{localDir}</div>
        <div class="sub">{fmt(freeBytes)} free</div>
        <button class="ghost" onclick={chooseFolder} disabled={busy}>Choose different folder</button>
      </div>
    </div>

    <div class="opts">
      <label class="opt" class:sel={mode === "everything"}>
        <input type="radio" bind:group={mode} value="everything" disabled={busy} />
        <span>Synchronize everything from server
          {#if info.accountBytes > 0}<em>({fmt(info.accountBytes)})</em>{/if}
        </span>
      </label>

      {#if mode === "everything"}
        <div class="firstline">
          {#if first.length}
            <span class="sub ftext" title={first.join(", ")}>Downloads first: <b>{firstSummary}</b></span>
            <button class="linkbtn" onclick={openPick} disabled={busy}>Change</button>
          {:else}
            <button class="linkbtn" onclick={openPick} disabled={busy}>Choose folders to download first (optional)</button>
          {/if}
        </div>
      {/if}

      <label class="opt" class:sel={mode === "choose"}>
        <input type="radio" bind:group={mode} value="choose" disabled={busy} />
        <span>Choose what to sync</span>
      </label>

      <label class="opt" class:sel={mode === "ondemand"} class:off={!info.onDemandSupport}>
        <input type="radio" bind:group={mode} value="ondemand" disabled={busy || !info.onDemandSupport} />
        <span>Use virtual files instead of downloading content immediately</span>
      </label>
      {#if mode === "ondemand"}
        <p class="note">Files appear instantly but download only when you open them.
          Anything you add or change syncs back to your server as usual.</p>
      {/if}
    </div>

    {#if err}<p class="err">{err}</p>{/if}

    <div class="actions">
      <button class="ghost" onclick={skip} disabled={busy}>Skip for now</button>
      <button class="primary" onclick={connect} disabled={busy || !localDir}>
        {busy ? "Setting up…" : "Connect"}
      </button>
    </div>

    {#if scanning}
      <div class="modalback">
        <div class="modalbox">
          <div class="mhead">Checking your files…</div>
          <p class="sub left">Nimbo is comparing everything in this folder with your server before setting up. On a large account this can take several minutes. Nothing is changed until you confirm.</p>
          <p class="sub left"><b>{scanDirs}</b> folders checked so far</p>
          <div class="actions">
            <button class="ghost" onclick={cancelScan}>Cancel</button>
          </div>
        </div>
      </div>
    {/if}

    {#if picking}
      <div class="modalback">
        <div class="modalbox pick">
          <div class="mhead">Download these first</div>
          <p class="sub left">Tick the folders you need straight away. They download before everything else, and the rest follows.</p>
          <div class="crumb">
            <button class="ghost small" onclick={pickUp} disabled={!pcur}>⬆ Up</button>
            <span class="sub path" title={"/" + pcur}>/{pcur}</span>
          </div>
          <div class="plist">
            {#if ploading}<p class="sub left">Loading…</p>
            {:else if pentries.length === 0}<p class="sub left">No folders here.</p>{/if}
            {#each pentries as e (e.path)}
              <div class="prow">
                <input type="checkbox" checked={first.includes(e.path)} onchange={() => toggleFirst(e.path)} />
                <button class="pname" onclick={() => pickGo(e.path)} title="Open {e.name}">📁 {e.name}</button>
              </div>
            {/each}
          </div>
          <div class="actions">
            <span class="sub count">{first.length} chosen</span>
            {#if first.length}<button class="ghost" onclick={() => (first = [])}>Clear</button>{/if}
            <button class="primary" onclick={() => (picking = false)}>Done</button>
          </div>
        </div>
      </div>
    {/if}

    {#if adopt}
      <div class="modalback">
        <div class="modalbox">
          <div class="mhead">Keep the files already in this folder?</div>
          <p class="sub left">There are already files in <b>{localDir}</b>. Nimbo can keep them where they are instead of downloading everything again.</p>
          <ul class="sub mlist">
            {#if adopt.keep}<li><b>{adopt.keep}</b> already {adopt.keep === 1 ? "matches" : "match"} your server — kept on this PC.</li>{/if}
            {#if adopt.replace}<li><b>{adopt.replace}</b> {adopt.replace === 1 ? "is an online-only leftover" : "are online-only leftovers"} from another app — replaced with {adopt.replace === 1 ? "a Nimbo placeholder" : "Nimbo placeholders"}.</li>{/if}
            {#if adopt.upload}<li><b>{adopt.upload}</b> {adopt.upload === 1 ? "isn’t" : "aren’t"} on your server yet — uploaded.</li>{/if}
            {#if adopt.conflict}<li><b>{adopt.conflict}</b> {adopt.conflict === 1 ? "differs" : "differ"} from your server — both versions kept.</li>{/if}
          </ul>
          {#if adopt.uploadBytes}<p class="sub left">Upload size: {fmt(adopt.uploadBytes)}</p>{/if}
          <div class="actions">
            <button class="ghost" onclick={cancelAdopt}>Cancel</button>
            <button class="primary" onclick={confirmAdopt}>Keep my files</button>
          </div>
        </div>
      </div>
    {/if}
  {:else}
    <p class="loading">Loading account…</p>
  {/if}
</div>

<style>
  .setup { height: 100%; box-sizing: border-box; padding: 22px; background: var(--bg); color: var(--fg);
           display: flex; flex-direction: column; gap: 18px; font-size: 13px; }
  .head { display: flex; align-items: flex-start; justify-content: center; gap: 18px; }
  .who { flex: 1; text-align: center; display: flex; flex-direction: column; align-items: center; gap: 4px; }
  .arrow { font-size: 22px; color: var(--fg2); align-self: center; }
  .avatar { width: 64px; height: 64px; border-radius: 50%; background: var(--accent); color: #fff;
            font-size: 28px; font-weight: 600; display: flex; align-items: center; justify-content: center; }
  .folder { font-size: 56px; line-height: 64px; }
  .name { font-weight: 600; font-size: 14px; }
  .sub { color: var(--fg2); font-size: 12px; }
  .path { max-width: 220px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .opts { display: flex; flex-direction: column; gap: 8px; }
  .opt { display: flex; align-items: flex-start; gap: 9px; padding: 9px 11px; border: 1px solid var(--border-2);
         border-radius: 8px; cursor: pointer; line-height: 1.35; }
  .opt.sel { border-color: var(--accent); background: color-mix(in srgb, var(--accent) 10%, transparent); }
  .opt.off { opacity: 0.5; cursor: not-allowed; }
  .opt input { margin-top: 2px; }
  .opt em { color: var(--fg2); font-style: normal; }
  .exp { font-size: 10px; text-transform: uppercase; letter-spacing: 0.5px; color: var(--accent);
         border: 1px solid var(--accent); border-radius: 4px; padding: 0 5px; margin-left: 6px; }
  .note { margin: -2px 2px 0; color: var(--fg2); font-size: 12px; }
  .err { color: #e5484d; margin: 0; }
  .loading { color: var(--fg2); }
  .actions { margin-top: auto; display: flex; justify-content: flex-end; gap: 10px; }
  button { padding: 9px 16px; border-radius: 6px; font-size: 13px; cursor: pointer; }
  button.primary { border: none; background: var(--accent); color: #fff; font-weight: 500; }
  button.primary:hover { background: var(--accent-dark); }
  button.ghost { border: 1px solid var(--border-2); background: transparent; color: var(--fg); }
  button.ghost:hover { background: var(--hover); }
  button:disabled { opacity: 0.6; cursor: default; }
  .modalback { position: fixed; inset: 0; background: rgba(0, 0, 0, 0.45);
               display: flex; align-items: center; justify-content: center; z-index: 10; }
  .modalbox { background: var(--bg); border: 1px solid var(--border-2); border-radius: 10px;
              padding: 18px; max-width: 420px; display: flex; flex-direction: column; gap: 10px; }
  .mhead { font-weight: 600; font-size: 14px; }
  .mlist { margin: 0; padding-left: 18px; display: flex; flex-direction: column; gap: 4px; }
  .left { text-align: left; }
  .modalbox .actions { margin-top: 4px; align-items: center; }
  .firstline { display: flex; align-items: center; gap: 6px; margin: -2px 2px 0 30px; min-width: 0; }
  .firstline .ftext { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .linkbtn { background: none; border: none; padding: 0; color: var(--accent); font-size: 12px; cursor: pointer; white-space: nowrap; }
  .linkbtn:hover { text-decoration: underline; }
  .modalbox.pick { width: 420px; max-height: 80vh; }
  .crumb { display: flex; align-items: center; gap: 8px; }
  .crumb .path { max-width: 300px; }
  button.small { padding: 4px 10px; font-size: 12px; }
  .plist { border: 1px solid var(--border-2); border-radius: 6px; padding: 4px 8px; overflow-y: auto; min-height: 120px; max-height: 300px; }
  .prow { display: flex; align-items: center; gap: 8px; padding: 4px 0; }
  .pname { background: none; border: none; padding: 0; color: var(--fg); text-align: left; cursor: pointer; font-size: 13px;
           overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .pname:hover { color: var(--accent); }
  .count { margin-right: auto; }
</style>
