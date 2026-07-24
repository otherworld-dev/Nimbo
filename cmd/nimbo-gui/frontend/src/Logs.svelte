<script lang="ts">
  import { App } from "../bindings/github.com/otherworld/nimbo/cmd/nimbo-gui";

  let text = $state("");
  let path = $state("");
  let verbose = $state(false);
  let copied = $state(false);
  let box: HTMLPreElement;

  async function load(scroll = true) {
    text = await App.TailLog();
    path = await App.LogPath();
    verbose = await App.Verbose();
    if (scroll) requestAnimationFrame(() => { if (box) box.scrollTop = box.scrollHeight; });
  }
  load();

  async function copy() {
    await App.CopyToClipboard(text);
    copied = true;
    setTimeout(() => (copied = false), 1500);
  }
  function toggleVerbose() { verbose = !verbose; App.SetVerbose(verbose); }
</script>

<div class="win">
  <header>
    <div class="lbl">Logs</div>
    <div class="path" title={path}>{path}</div>
    <div class="bar">
      <button onclick={() => load()}>Refresh</button>
      <button onclick={copy}>{copied ? "Copied!" : "Copy"}</button>
      <button onclick={() => App.OpenLogFolder()}>Open folder</button>
      <label class="chk"><input type="checkbox" checked={verbose} onchange={toggleVerbose} /> Verbose</label>
    </div>
  </header>
  <pre class="log" bind:this={box}>{text || "(no log yet)"}</pre>
</div>

<style>
  .win { height: 100%; display: flex; flex-direction: column; background: var(--bg); color: var(--fg); }
  header { padding: 12px 14px; border-bottom: 1px solid var(--border); }
  .lbl { font-size: 11px; font-weight: 600; letter-spacing: .5px; text-transform: uppercase; color: var(--muted); }
  .path { font-size: 12px; color: var(--muted); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; margin: 2px 0 8px; }
  .bar { display: flex; align-items: center; gap: 8px; }
  .bar button { padding: 6px 11px; border: 1px solid var(--border-2); border-radius: 6px; background: var(--panel-2); color: var(--fg); cursor: pointer; font-size: 12.5px; }
  .bar button:hover { background: var(--hover); }
  .chk { display: flex; align-items: center; gap: 6px; font-size: 12.5px; color: var(--fg2); margin-left: auto; }
  .log { flex: 1; margin: 0; overflow: auto; padding: 10px 14px; font-family: Consolas, "Cascadia Mono", monospace;
         font-size: 11.5px; line-height: 1.5; color: var(--fg); background: var(--panel); white-space: pre-wrap; word-break: break-word; }
</style>
