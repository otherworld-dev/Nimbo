<script lang="ts">
  import { Events } from "@wailsio/runtime";
  import { App } from "../bindings/github.com/otherworld/nimbo/cmd/nimbo-gui";

  type Share = { id: string; type: number; shareWith: string; url: string; permissions: number; expiration: string };

  let path = $state("");
  let shares = $state<Share[]>([]);
  let err = $state("");
  let busy = $state(false);
  let copied = $state("");

  // Public-link form
  let linkPw = $state("");
  let linkExp = $state("");
  let linkEdit = $state(false);
  // User-share form
  let userName = $state("");
  let userEdit = $state(false);

  async function load() {
    path = await App.ShareTarget();
    shares = (await App.ShareList()) ?? [];
  }
  load();
  Events.On("share:target", () => { err = ""; copied = ""; load(); });

  const basename = (p: string) => p.replace(/\/+$/, "").split("/").pop() || p;
  const canEdit = (perm: number) => (perm & 2) !== 0; // PermUpdate
  const isPublic = (t: number) => t === 3;

  async function run(fn: () => Promise<string>) {
    err = ""; busy = true;
    const e = await fn();
    busy = false;
    if (e) { err = e; return false; }
    await load();
    return true;
  }

  async function createLink() {
    if (await run(() => App.CreatePublicLink(linkPw.trim(), linkExp.trim(), linkEdit))) {
      linkPw = ""; linkExp = ""; linkEdit = false;
    }
  }
  async function createUser() {
    if (await run(() => App.CreateUserShare(userName, userEdit))) {
      userName = ""; userEdit = false;
    }
  }
  const remove = (id: string) => run(() => App.DeleteShare(id));

  async function copy(url: string, id: string) {
    await App.CopyToClipboard(url);
    copied = id;
    setTimeout(() => { if (copied === id) copied = ""; }, 1500);
  }
  const open = (url: string) => App.OpenURL(url);
</script>

<div class="win">
  <header>
    <div class="lbl">Sharing</div>
    <div class="path" title={path}>📄 {basename(path)}</div>
    <div class="sub">/{path}</div>
  </header>

  <div class="body">
    {#if err}<p class="err">{err}</p>{/if}

    <h3>Shares</h3>
    {#if shares.length === 0}
      <p class="empty">Not shared yet.</p>
    {:else}
      {#each shares as s}
        <div class="share">
          <div class="srow">
            <span class="who">
              {#if isPublic(s.type)}🔗 Public link{:else}👤 {s.shareWith}{/if}
            </span>
            <span class="perm">{canEdit(s.permissions) ? "Can edit" : "View only"}</span>
            <button class="link danger" onclick={() => remove(s.id)} disabled={busy}>Remove</button>
          </div>
          {#if isPublic(s.type) && s.url}
            <div class="urlrow">
              <input class="url" readonly value={s.url} />
              <button onclick={() => copy(s.url, s.id)}>{copied === s.id ? "Copied!" : "Copy"}</button>
              <button onclick={() => open(s.url)}>Open</button>
            </div>
          {/if}
          {#if s.expiration}<div class="exp">Expires {s.expiration.slice(0, 10)}</div>{/if}
        </div>
      {/each}
    {/if}

    <h3>Create public link</h3>
    <div class="form">
      <div class="frow"><label>Password (optional)</label><input type="password" bind:value={linkPw} placeholder="No password" /></div>
      <div class="frow"><label>Expires (optional)</label><input type="date" bind:value={linkExp} /></div>
      <label class="chk"><input type="checkbox" bind:checked={linkEdit} /> Allow editing &amp; uploads</label>
      <button class="primary" onclick={createLink} disabled={busy}>Create public link</button>
    </div>

    <h3>Share with a user</h3>
    <div class="form">
      <div class="frow"><label>Username</label><input bind:value={userName} placeholder="e.g. alice"
            onkeydown={(e) => e.key === 'Enter' && createUser()} /></div>
      <label class="chk"><input type="checkbox" bind:checked={userEdit} /> Allow editing &amp; uploads</label>
      <button class="primary" onclick={createUser} disabled={busy}>Share</button>
    </div>
  </div>
</div>

<style>
  .win { height: 100%; display: flex; flex-direction: column; background: var(--bg); color: var(--fg); }
  header { padding: 14px 16px; border-bottom: 1px solid var(--border); }
  .lbl { font-size: 11px; font-weight: 600; letter-spacing: .5px; text-transform: uppercase; color: var(--muted); }
  .path { font-size: 16px; font-weight: 600; margin-top: 2px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .sub { font-size: 12px; color: var(--muted); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .body { flex: 1; overflow-y: auto; padding: 14px 16px; }
  h3 { font-size: 12px; text-transform: uppercase; color: var(--muted); margin: 18px 0 8px; }
  h3:first-of-type { margin-top: 0; }
  .empty { color: var(--muted); font-size: 13px; }
  .err { color: #c0392b; font-size: 12.5px; background: #fdeceb; border: 1px solid #f0c0bb; border-radius: 6px; padding: 8px 10px; margin: 0 0 12px; }
  .share { border: 1px solid var(--border); border-radius: 8px; padding: 10px; margin-bottom: 8px; background: var(--panel); }
  .srow { display: flex; align-items: center; gap: 10px; }
  .who { flex: 1; font-size: 13px; font-weight: 500; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .perm { font-size: 11px; color: var(--fg2); background: var(--hover); border-radius: 10px; padding: 2px 8px; white-space: nowrap; }
  .urlrow { display: flex; gap: 6px; margin-top: 8px; }
  .url { flex: 1; padding: 6px 8px; border: 1px solid var(--border-2); border-radius: 5px; font-size: 12px; background: var(--bg); color: var(--fg); }
  .urlrow button { padding: 6px 10px; border: 1px solid var(--border-2); border-radius: 5px; background: var(--bg); color: var(--fg); cursor: pointer; font-size: 12px; }
  .urlrow button:hover { background: var(--hover); }
  .exp { font-size: 11px; color: var(--muted); margin-top: 6px; }
  .form { display: flex; flex-direction: column; gap: 10px; max-width: 360px; }
  .frow { display: flex; flex-direction: column; gap: 4px; }
  .frow label { font-size: 12px; color: var(--fg2); }
  .frow input { padding: 7px 8px; border: 1px solid var(--border-2); border-radius: 5px; font-size: 13px; background: var(--bg); color: var(--fg); }
  .chk { display: flex; align-items: center; gap: 7px; font-size: 13px; color: var(--fg2); }
  .primary { align-self: flex-start; padding: 8px 16px; border: none; border-radius: 6px; background: var(--accent); color: #fff; cursor: pointer; font-size: 13px; }
  .primary:hover { background: var(--accent-dark); }
  .primary:disabled { opacity: .6; cursor: default; }
  .link { background: none; border: none; color: var(--accent); cursor: pointer; font-size: 12px; }
  .link.danger { color: #c0392b; }
  .link:hover { text-decoration: underline; }
</style>
