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

  async function connect() {
    busy = true; err = "";
    const msg = await App.CompleteSetup(localDir, mode);
    if (msg) { err = msg; busy = false; return; }
    if (mode === "choose") await App.OpenSettings();
    done();
  }

  function skip() { done(); }
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

      <label class="opt" class:sel={mode === "choose"}>
        <input type="radio" bind:group={mode} value="choose" disabled={busy} />
        <span>Choose what to sync</span>
      </label>

      <label class="opt" class:sel={mode === "ondemand"} class:off={!info.onDemandSupport}>
        <input type="radio" bind:group={mode} value="ondemand" disabled={busy || !info.onDemandSupport} />
        <span>Use virtual files instead of downloading content immediately
          <em class="exp">experimental</em>
        </span>
      </label>
      {#if mode === "ondemand"}
        <p class="note">Files appear instantly but download only when you open them.
          Changes you make aren't uploaded back yet.</p>
      {/if}
    </div>

    {#if err}<p class="err">{err}</p>{/if}

    <div class="actions">
      <button class="ghost" onclick={skip} disabled={busy}>Skip for now</button>
      <button class="primary" onclick={connect} disabled={busy || !localDir}>
        {busy ? "Setting up…" : "Connect"}
      </button>
    </div>
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
</style>
