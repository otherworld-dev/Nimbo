<script lang="ts">
  import { Events } from "@wailsio/runtime";
  import { App } from "../bindings/github.com/otherworld/nimbo/cmd/nimbo-gui";
  import Setup from "./Setup.svelte";

  let server = $state("");
  let msg = $state("Enter your Nextcloud server address to begin.");
  let busy = $state(false);
  let phase = $state<"signin" | "setup">("signin");

  Events.On("login:error", (e: any) => { if (!busy) return; msg = "Sign-in failed: " + e.data; busy = false; });
  Events.On("login:done", async () => {
    busy = false;
    await App.EnterSetup(); // grow the window for the configuration screen
    phase = "setup";
  });

  async function login() {
    if (!server.trim()) return;
    busy = true;
    msg = "Connecting…";
    const url = await App.BeginLogin(server);
    if (url.startsWith("error:")) { msg = url; busy = false; return; }
    msg = "Approve access in the browser that just opened, then come back here.";
  }

  // Give up on a browser sign-in the user abandoned: re-enable the form so they
  // can retry (the next Log in supersedes the pending poll; closing the window
  // also stops it).
  function cancel() {
    busy = false;
    msg = "Enter your Nextcloud server address to begin.";
  }
</script>

{#if phase === "setup"}
  <Setup done={() => App.CloseLogin()} />
{:else}
  <div class="login">
    <h1>Sign in to Nextcloud</h1>
    <input placeholder="https://cloud.example.com" bind:value={server} disabled={busy}
           onkeydown={(e) => e.key === "Enter" && login()} />
    <button class="primary" onclick={login} disabled={busy}>Log in</button>
    {#if busy}
      <button class="secondary" onclick={cancel}>Cancel</button>
    {/if}
    <p class="msg">{msg}</p>
  </div>
{/if}

<style>
  .login { height: 100%; background: var(--bg); color: var(--fg); padding: 22px; box-sizing: border-box;
           display: flex; flex-direction: column; gap: 12px; }
  h1 { font-size: 17px; margin: 0; }
  input { padding: 9px 10px; border: 1px solid var(--border-2); border-radius: 6px; font-size: 13px; background: var(--bg); color: var(--fg); }
  button.primary { padding: 9px; border: none; border-radius: 6px; background: var(--accent); color: #fff;
                   font-size: 13px; font-weight: 500; cursor: pointer; }
  button.primary:hover { background: var(--accent-dark); }
  button.secondary { padding: 8px; border: 1px solid var(--border-2); border-radius: 6px; background: transparent;
                     color: var(--fg2); font-size: 13px; cursor: pointer; }
  button.secondary:hover { color: var(--fg); border-color: var(--fg2); }
  button:disabled { opacity: 0.6; cursor: default; }
  .msg { color: var(--fg2); font-size: 13px; margin: 0; }
</style>
