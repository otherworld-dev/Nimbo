<script lang="ts">
  import { dialog, closeDialog } from "./dialogs.svelte";

  let text = $state("");
  let okBtn = $state<HTMLButtonElement | null>(null);
  let input = $state<HTMLInputElement | null>(null);

  // Focus the answer when a dialog opens, so Enter and Escape work straight away.
  $effect(() => {
    const d = dialog.current;
    if (!d) return;
    text = d.value;
    queueMicrotask(() => {
      if (d.kind === "text" && input) { input.focus(); input.select(); }
      else okBtn?.focus();
    });
  });

  function ok() {
    const d = dialog.current;
    if (!d) return;
    closeDialog(d.kind === "ask" ? true : d.kind === "text" ? text : undefined);
  }
  function cancel() {
    const d = dialog.current;
    if (!d) return;
    closeDialog(d.kind === "ask" ? false : d.kind === "text" ? null : undefined);
  }
  function onKey(e: KeyboardEvent) {
    if (!dialog.current) return;
    if (e.key === "Escape") { e.preventDefault(); cancel(); }
    else if (e.key === "Enter" && dialog.current.kind === "text") { e.preventDefault(); ok(); }
  }
</script>

<svelte:window onkeydown={onKey} />

{#if dialog.current}
  {@const d = dialog.current}
  <div class="dlgback">
    <div class="dlgbox" role="dialog" aria-modal="true" aria-labelledby="dlgtitle">
      <h3 id="dlgtitle">{d.title}</h3>
      {#if d.message}<p class="dlgmsg">{d.message}</p>{/if}
      {#if d.kind === "text"}
        <input class="dlginput" bind:this={input} bind:value={text} />
      {/if}
      <div class="dlgbtns">
        {#if d.kind !== "tell"}<button onclick={cancel}>{d.cancel}</button>{/if}
        <button class="primary" class:danger={d.danger} bind:this={okBtn} onclick={ok}>{d.ok}</button>
      </div>
    </div>
  </div>
{/if}

<style>
  .dlgback { position: fixed; inset: 0; background: rgba(0,0,0,.45); display: flex;
             align-items: center; justify-content: center; z-index: 1000; }
  .dlgbox { width: 90%; max-width: 380px; max-height: 90%; overflow-y: auto; padding: 16px;
            background: var(--bg); color: var(--fg); border: 1px solid var(--border);
            border-radius: 8px; box-shadow: 0 10px 40px rgba(0,0,0,.4); }
  h3 { margin: 0 0 8px; font-size: 14px; font-weight: 600; }
  .dlgmsg { margin: 0 0 12px; font-size: 12.5px; line-height: 1.45; color: var(--fg2);
            white-space: pre-line; overflow-wrap: anywhere; }
  .dlginput { width: 100%; margin: 0 0 12px; padding: 7px 8px; border: 1px solid var(--border-2);
              border-radius: 5px; background: var(--panel); color: var(--fg); font: inherit; font-size: 13px; }
  .dlginput:focus { outline: none; border-color: var(--accent); }
  .dlgbtns { display: flex; justify-content: flex-end; gap: 8px; }
  .dlgbtns button { padding: 6px 14px; border: 1px solid var(--border-2); border-radius: 6px;
                    background: var(--panel-2); color: var(--fg); cursor: pointer; font-size: 12.5px; }
  .dlgbtns button:hover { background: var(--hover); }
  .dlgbtns button.primary { background: var(--accent); border-color: var(--accent); color: #fff; }
  .dlgbtns button.primary:hover { background: var(--accent-dark); }
  .dlgbtns button.primary.danger { background: #c0392b; border-color: #c0392b; }
  .dlgbtns button.primary.danger:hover { background: #a93226; }
  .dlgbtns button:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
</style>
