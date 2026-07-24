<script lang="ts">
  // Branded loading screen for app windows: shown instantly while the real
  // Nextcloud app page is fetched (the Go side redirects this window to the
  // app URL once this page has rendered; the splash stays visible until the
  // app's document commits). Route: #appsplash/<name>?icon=<url>
  const hash = window.location.hash.slice(1); // "appsplash/Mail?icon=..."
  const rest = hash.startsWith("appsplash/") ? hash.slice("appsplash/".length) : "";
  const qi = rest.indexOf("?");
  const name = decodeURIComponent(qi >= 0 ? rest.slice(0, qi) : rest) || "App";
  const icon = qi >= 0 ? new URLSearchParams(rest.slice(qi)).get("icon") || "" : "";
</script>

<div class="splash">
  <div class="tile">
    {#if icon}<img src={icon} alt="" />{:else}<span class="glyph">🗂</span>{/if}
  </div>
  <div class="name">{name}</div>
  <div class="dots"><span></span><span></span><span></span></div>
  <div class="hint">Loading from your server…</div>
</div>

<style>
  .splash { height: 100vh; display: flex; flex-direction: column; align-items: center; justify-content: center;
            gap: 14px; background: var(--bg); color: var(--fg); }
  .tile { width: 72px; height: 72px; border-radius: 18px; background: var(--accent);
          display: flex; align-items: center; justify-content: center;
          box-shadow: 0 14px 38px -14px color-mix(in srgb, var(--accent) 70%, transparent); }
  .tile img { width: 40px; height: 40px; filter: brightness(0) invert(1); }
  .glyph { font-size: 34px; }
  .name { font-size: 19px; font-weight: 700; }
  .dots { display: flex; gap: 6px; }
  .dots span { width: 7px; height: 7px; border-radius: 50%; background: var(--accent); opacity: .25;
               animation: pulse 1.2s ease-in-out infinite; }
  .dots span:nth-child(2) { animation-delay: .2s; }
  .dots span:nth-child(3) { animation-delay: .4s; }
  @keyframes pulse { 0%, 100% { opacity: .25; transform: scale(1); } 50% { opacity: 1; transform: scale(1.25); } }
  .hint { font-size: 12.5px; color: var(--muted); }
</style>
