<script lang="ts">
  import { Events } from "@wailsio/runtime";
  import { App } from "../bindings/github.com/otherworld/nimbo/cmd/nimbo-gui";
  import Setup from "./Setup.svelte";

  const startMsg = "Enter your Nextcloud server address to begin.";
  let server = $state("");
  let msg = $state(startMsg);
  let busy = $state(false);
  let phase = $state<"signin" | "setup">("signin");

  // An account signing in again rather than a new one (GitHub #20): the window
  // names it and fills in its server, so it doesn't look like adding an account
  // that might overwrite the one already set up. "different" drops back to a
  // plain sign-in for some other account.
  let target = $state<{ id: string; address: string; server: string } | null>(null);
  let different = $state(false);
  const again = $derived(!!target && !different);

  async function loadTarget() {
    const t = await App.SignInTarget();
    target = t?.id ? t : null;
    different = false;
    if (!busy) {
      server = target ? target.server : "";
      msg = target ? "" : startMsg;
    }
  }
  loadTarget();
  Events.On("login:target", loadTarget);

  Events.On("login:error", (e: any) => { if (!busy) return; msg = "Sign-in failed: " + e.data; busy = false; });
  Events.On("login:done", async (e: any) => {
    busy = false;
    // An account Nimbo already has keeps its folders and settings: no setup.
    if (e.data === true) {
      msg = "Signed in again.";
      await App.CloseLogin();
      return;
    }
    await App.EnterSetup(); // grow the window for the configuration screen
    phase = "setup";
  });

  async function login() {
    if (!server.trim()) return;
    busy = true;
    msg = "Connecting…";
    const url = await App.BeginLogin(server);
    if (url.startsWith("error:")) { msg = url; busy = false; return; }
    // The window minimises itself meanwhile (it covered the browser's Grant
    // access button) and comes back for setup or an error.
    msg = "Finish signing in in the browser that just opened. This window steps aside until you have.";
  }

  // Give up on a browser sign-in the user abandoned: re-enable the form so they
  // can retry (the next Log in supersedes the pending poll; closing the window
  // also stops it).
  function cancel() {
    busy = false;
    msg = again ? "" : startMsg;
  }

  function signInSomeoneElse() {
    App.SignInSomeoneElse(); // the grant may now be for any account
    different = true;
    server = "";
    msg = startMsg;
  }
</script>

{#if phase === "setup"}
  <Setup done={() => App.CloseLogin()} />
{:else}
  <div class="login">
    {#if again && target}
      <h1>Sign in again</h1>
      <p class="who">{target.address}</p>
      <p class="note">Nimbo can't sign in to this account any more, for example because its access was
        removed in Nextcloud. Sign in again and it carries on where it left off: your folders and
        settings are kept, and nothing needs setting up again.</p>
      <button class="primary" onclick={login} disabled={busy}>Sign in again</button>
    {:else}
      <h1>Sign in to Nextcloud</h1>
      <input placeholder="https://cloud.example.com" bind:value={server} disabled={busy}
             onkeydown={(e) => e.key === "Enter" && login()} />
      <button class="primary" onclick={login} disabled={busy}>Log in</button>
    {/if}
    {#if busy}
      <button class="secondary" onclick={cancel}>Cancel</button>
    {/if}
    {#if msg}<p class="msg">{msg}</p>{/if}
    {#if again && !busy}
      <button class="link" onclick={signInSomeoneElse}>Sign in to a different account instead</button>
    {/if}
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
  .who { margin: -4px 0 0; font-size: 13px; font-weight: 600; color: var(--fg); word-break: break-all;
         user-select: text; -webkit-user-select: text; }
  .note { margin: 0; font-size: 12.5px; line-height: 1.45; color: var(--fg2); }
  button.link { align-self: flex-start; padding: 0; border: none; background: none; color: var(--accent);
                font-size: 12.5px; cursor: pointer; }
  button.link:hover { text-decoration: underline; }
</style>
