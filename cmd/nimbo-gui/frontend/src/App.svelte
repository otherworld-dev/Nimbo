<script lang="ts">
  import Flyout from "./Flyout.svelte";
  import Status from "./Status.svelte";
  import Settings from "./Settings.svelte";
  import Login from "./Login.svelte";
  import Share from "./Share.svelte";
  import Versions from "./Versions.svelte";
  import Logs from "./Logs.svelte";
  import AppSplash from "./AppSplash.svelte";
  import { Events } from "@wailsio/runtime";
  import { App } from "../bindings/github.com/otherworld/nimbo/cmd/nimbo-gui";

  // Each Wails window loads a different hash route.
  let route = $state(window.location.hash.replace("#", ""));
  window.addEventListener("hashchange", () => { route = window.location.hash.replace("#", ""); });

  // Light/dark theme. Modes: "light", "dark", or "system" (labelled "Match
  // Nextcloud theme"): use the user's explicit Nextcloud appearance when set,
  // otherwise — if their Nextcloud follows the system — track the OS via
  // prefers-color-scheme. Re-applied live when changed in settings.
  const sysDark = window.matchMedia("(prefers-color-scheme: dark)");
  let themeMode = "system";
  let ncAppearance = ""; // "dark" | "light" | "default" | "" (not yet loaded)
  function applyTheme() {
    let dark: boolean;
    if (themeMode === "dark") dark = true;
    else if (themeMode === "light") dark = false;
    else if (ncAppearance === "dark") dark = true;       // Nextcloud: dark
    else if (ncAppearance === "light") dark = false;     // Nextcloud: light
    else dark = sysDark.matches;                         // default/unknown -> OS
    if (dark) document.documentElement.setAttribute("data-theme", "dark");
    else document.documentElement.removeAttribute("data-theme");
  }
  sysDark.addEventListener("change", applyTheme);
  Events.On("theme", (e: any) => { themeMode = e.data || "system"; applyTheme(); });
  (async () => { themeMode = await App.GetTheme(); applyTheme(); })();
  // Fetch the Nextcloud appearance once the engine is up (retry briefly), then
  // re-apply so "Match Nextcloud theme" reflects the server's light/dark choice.
  let appearanceGen = 0; // a newer fetch supersedes any loop still retrying
  async function fetchNcAppearance() {
    const gen = ++appearanceGen;
    for (let i = 0; i < 12 && gen === appearanceGen; i++) {
      const a = await App.NextcloudAppearance();
      if (a) { ncAppearance = a; applyTheme(); return; }
      await new Promise(r => setTimeout(r, 1000));
    }
  }
  fetchNcAppearance();

  // Accent the UI with the user's Nextcloud theme colour (every window applies it).
  function darken(hex: string, f: number): string {
    const m = hex.replace("#", "");
    if (m.length !== 6) return hex;
    const c = [0, 2, 4].map(i => Math.max(0, Math.round(parseInt(m.slice(i, i + 2), 16) * f)));
    return "#" + c.map(x => x.toString(16).padStart(2, "0")).join("");
  }
  function applyAccent(hex: string) {
    const root = document.documentElement;
    if (hex && /^#[0-9a-fA-F]{6}$/.test(hex)) {
      root.style.setProperty("--accent", hex);
      root.style.setProperty("--accent-dark", darken(hex, 0.88));
    } else {
      root.style.removeProperty("--accent"); // stylesheet default
      root.style.removeProperty("--accent-dark");
    }
  }
  // The engine pushes the theme when it starts (this window may load long before
  // that — on a fresh install the flyout exists while the user is still signing
  // in, and on an account switch the colour changes). Re-read the appearance too.
  Events.On("accent", (e: any) => { applyAccent(e.data || ""); fetchNcAppearance(); });
  (async () => {
    // The engine (which fetches the theme) may not be ready at window load, so
    // retry briefly until a colour is available.
    for (let i = 0; i < 12; i++) {
      try {
        const hex = await App.ThemeColor();
        if (hex && /^#[0-9a-fA-F]{6}$/.test(hex)) {
          applyAccent(hex);
          return;
        }
      } catch { /* default accent */ }
      await new Promise(r => setTimeout(r, 1000));
    }
  })();
</script>

{#if route.startsWith("appsplash")}
  <AppSplash />
{:else if route === "status"}
  <Status />
{:else if route === "settings"}
  <Settings />
{:else if route === "login"}
  <Login />
{:else if route === "share"}
  <Share />
{:else if route === "versions"}
  <Versions />
{:else if route === "logs"}
  <Logs />
{:else}
  <Flyout />
{/if}
