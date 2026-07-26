<script lang="ts">
  import { App } from "../bindings/github.com/otherworld/nimbo/cmd/nimbo-gui";
  import { Events } from "@wailsio/runtime";

  let brandName = $state("Nimbo");
  let brandCompany = $state("Otherworld Dev Ltd");
  let brandWebsite = $state("https://www.nimbosync.com");
  let brandSupport = $state("contact@otherworld.dev");
  (async () => {
    const b = await App.Brand();
    brandName = b.name || brandName; brandCompany = b.company || brandCompany;
    brandWebsite = b.website || brandWebsite; brandSupport = b.support || brandSupport;
  })();
  const hostname = (u: string) => { try { return new URL(u).host.replace(/^www\./, ""); } catch { return u; } };

  // Admin policy (managed deployments): some settings may be locked by IT.
  let policy = $state<{ managed: boolean; lockServer: boolean; serverUrl: string; allowSignOut: boolean; lockBandwidth: boolean; lockSyncMode: boolean }>(
    { managed: false, lockServer: false, serverUrl: "", allowSignOut: true, lockBandwidth: false, lockSyncMode: false });
  (async () => { policy = await App.PolicyInfo(); })();

  type Entry = { name: string; path: string; isDir: boolean };
  type Pair = { localDir: string; remoteRoot: string; excludes: string[] };

  let tab = $state<"folders" | "sync" | "exclusions" | "appearance" | "general">("folders");

  // Folders
  let cur = $state("");
  let entries = $state<Entry[]>([]);
  let pairs = $state<Pair[]>([]);
  let synced = $state<string[]>([]);
  let baseDir = $state("");

  // Add-a-sync flow: select one or more remote folders, then sync them all into
  // a single local parent folder (each as a subfolder).
  let adding = $state(false);                       // false = list view, true = add flow
  let step = $state<"browse" | "dest">("browse");
  let selected = $state<string[]>([]);              // remote paths chosen to sync
  let parent = $state("");                          // local parent folder
  let names = $state<Record<string, string>>({});   // remote path -> local subfolder name
  let addErr = $state("");
  let busy = $state(false);
  let everything = $state(false);                   // syncing the whole account into one folder

  // Manage flow: deselect (or re-select) subfolders of an existing connection.
  let managing = $state(false);
  let managePair = $state<Pair | null>(null);
  let mCur = $state("");
  let mEntries = $state<Entry[]>([]);
  // When unticking a synced folder, ask whether to also delete the local copy.
  let pending = $state<{ localDir: string; rel: string; name: string } | null>(null);

  async function loadFolders() {
    pairs = (await App.GetPairs()) ?? [];
    synced = (await App.SyncedRemotes()) ?? [];
    baseDir = await App.GetBaseDir();
    if (adding && step === "browse") entries = (await App.BrowseRemote(cur)) ?? [];
  }
  loadFolders();

  async function startAdding() {
    adding = true; step = "browse"; selected = []; addErr = ""; cur = ""; everything = false;
    await loadFolders();
  }
  function exitAdding() { adding = false; step = "browse"; selected = []; addErr = ""; everything = false; }

  // Sync the whole account into one local folder (no per-folder picking).
  function chooseEverything() {
    everything = true;
    selected = [];
    parent = baseDir;
    addErr = "";
    step = "dest";
  }

  const trim = (s: string) => s.replace(/^\/+|\/+$/g, "");
  const base = (p: string) => trim(p).split("/").pop() || trim(p);
  const localName = (d: string) => d.replace(/[\\/]+$/, "").split(/[\\/]/).pop() || d;
  const coveringPair = (p: string): Pair | undefined =>
    pairs.find(pr => { const r = trim(pr.remoteRoot); return r !== p && (r === "" || p.startsWith(r + "/")); });
  const relWithin = (p: string, root: string) => (root === "" ? p : p.slice(trim(root).length + 1));

  function folderState(p: string): "synced" | "covered" | "none" {
    if (synced.includes(p)) return "synced";
    return coveringPair(p) ? "covered" : "none";
  }

  // --- Manage an existing connection: deselect/re-select its subfolders ---
  async function startManage(p: Pair) {
    managing = true; managePair = p; mCur = trim(p.remoteRoot); pending = null; addErr = "";
    await loadManage();
  }
  function exitManage() { managing = false; managePair = null; pending = null; }
  async function loadManage() {
    await loadFolders(); // refresh pairs so excludes reflect the latest toggles
    if (managePair) managePair = pairs.find(pr => pr.localDir === managePair!.localDir) ?? managePair;
    mEntries = (await App.BrowseRemote(mCur)) ?? [];
  }
  const mNavigate = async (p: string) => { mCur = trim(p); await loadManage(); };
  function mUp() {
    if (managePair && trim(mCur) !== trim(managePair.remoteRoot))
      mNavigate(mCur.split("/").slice(0, -1).join("/"));
  }
  // A folder is "on" unless the managed pair excludes it. While its keep/delete
  // prompt is open it shows unticked, so cancelling re-ticks it (and a later click
  // is a fresh untick rather than a stale toggle that re-asks).
  function mChecked(p: string): boolean {
    if (!managePair) return false;
    const rel = relWithin(trim(p), trim(managePair.remoteRoot));
    if (pending && pending.localDir === managePair.localDir && pending.rel === rel) return false;
    return !managePair.excludes.includes(rel);
  }
  async function onToggleManage(e: Entry) {
    if (!managePair) return;
    const rel = relWithin(trim(e.path), trim(managePair.remoteRoot));
    if (managePair.excludes.includes(rel)) {
      await App.RemoveExclude(managePair.localDir, rel); // re-select → re-downloads from the server
      await loadManage();
    } else {
      pending = { localDir: managePair.localDir, rel, name: e.name }; // ask keep vs delete
    }
  }
  async function confirmDeselect(deleteLocal: boolean) {
    if (!pending) return;
    const err = await App.DeselectFolder(pending.localDir, pending.rel, deleteLocal);
    pending = null;
    if (err) addErr = err;
    await loadManage();
  }

  function toggleSelect(path: string) {
    const t = trim(path);
    selected = selected.includes(t) ? selected.filter(x => x !== t) : [...selected, t];
  }
  // Move to the destination step, seeding the parent and a subfolder name per pick.
  function toDest() {
    if (!selected.length) return;
    parent = baseDir;
    const m: Record<string, string> = {};
    for (const r of selected) m[r] = base(r);
    names = m;
    addErr = "";
    step = "dest";
  }
  async function browseParent() {
    const p = await App.PickLocalFolder(parent || baseDir);
    if (p) parent = p;
  }
  function joinLocal(name: string): string {
    const b = parent.replace(/[\\/]+$/, "");
    const sep = b.includes("\\") ? "\\" : "/";
    return b ? b + sep + name : name;
  }
  async function confirmAdd() {
    busy = true; addErr = "";
    if (everything) {
      const dir = parent.trim();
      const err = !dir ? "Choose a local folder" : await App.AddSyncPair(dir, "");
      busy = false;
      if (err) { addErr = err; return; }
      exitAdding();
      await loadFolders();
      return;
    }
    const errs: string[] = [];
    for (const r of selected) {
      // If this folder is already synced by a broader connection (e.g. the whole-
      // account one), pull it out of that connection first — removing its old local
      // copy so it isn't kept in two places — then add it to the new group. The
      // server copy is always kept; the folder simply re-downloads into its new home.
      const cov = coveringPair(r);
      if (cov) {
        const derr = await App.DeselectFolder(cov.localDir, relWithin(r, trim(cov.remoteRoot)), true);
        if (derr) { errs.push(`/${r}: ${derr}`); continue; }
      }
      const err = await App.AddSyncPair(joinLocal((names[r] || base(r)).trim()), r);
      if (err) errs.push(`/${r}: ${err}`);
    }
    busy = false;
    if (errs.length) { addErr = errs.join("\n"); await loadFolders(); return; }
    exitAdding();
    await loadFolders();
  }
  async function removePair(p: Pair) {
    await App.RemoveSyncFolder(trim(p.remoteRoot), false); // keep local files
    await loadFolders();
  }
  let moveBusy = $state(false);
  // Disable "Move…" while a sync is active. The engine also refuses a mid-sync
  // move, but disabling the button prevents the click and explains why. Driven by
  // the live progress event — no polling.
  let syncActive = $state(false);
  (async () => { syncActive = !!(await App.Progress()).active; })();
  Events.On("progress", (e: any) => { syncActive = !!(e?.data?.active); });
  async function movePair(p: Pair) {
    if (moveBusy) return;
    // Ask for the PARENT folder to move into, then append this sync folder's own
    // name — so you pick WHERE and Nimbo makes the correctly-named subfolder. This
    // avoids accidentally landing on a populated folder (the "already has files"
    // refusal) and never merges into someone else's data.
    const curParent = p.localDir.replace(/[\\/][^\\/]*$/, "") || p.localDir;
    const newParent = await App.PickLocalFolder(curParent);
    if (!newParent) return;
    const sep = newParent.includes("\\") ? "\\" : "/";
    const dest = newParent.replace(/[\\/]+$/, "") + sep + localName(p.localDir);
    if (dest === p.localDir) return; // same place — nothing to do
    if (!confirm(`Move this sync folder to:\n\n${dest}\n\nNimbo moves your files there itself and keeps syncing — nothing is re-downloaded. Don't move the folder yourself in Explorer while Nimbo is running.`)) return;
    moveBusy = true;
    const err = await App.MoveSyncFolder(p.localDir, dest);
    moveBusy = false;
    if (err) { alert("Couldn't move the folder:\n\n" + err); return; }
    await loadFolders();
  }

  const navigate = async (p: string) => { cur = trim(p); await loadFolders(); };
  const up = () => { if (cur) navigate(cur.split("/").slice(0, -1).join("/")); };
  // Auto-save feedback: flash a small "Saved ✓" next to the control that just
  // persisted (keyed so several controls don't fight over one indicator).
  let savedFlash = $state("");
  let savedTimer: ReturnType<typeof setTimeout> | undefined;
  function flashSaved(key: string) {
    savedFlash = key;
    clearTimeout(savedTimer);
    savedTimer = setTimeout(() => (savedFlash = ""), 1500);
  }
  const saveBase = () => { App.SetBaseDir(baseDir.trim()); flashSaved("base"); };

  // Bandwidth
  let up_ = $state(0), down_ = $state(0);
  (async () => { const l = await App.GetLimits(); up_ = l.up; down_ = l.down; })();
  const saveLimits = () => { App.SetLimits(Number(up_) || 0, Number(down_) || 0); flashSaved("limits"); };

  // Exclusions: global ignore patterns (globs excluded from every sync pair, on
  // top of built-in defaults) and allowed filenames (normally-blocked names like
  // .htaccess the user wants to sync anyway).
  let ignore = $state<string[]>([]);
  let newPat = $state("");
  (async () => { ignore = (await App.GetIgnorePatterns()) ?? []; })();
  // Built-in default excludes — kept in sync with internal/engine/ignore.go's
  // defaultIgnore; shown read-only so "sync everything" is honest about them.
  const builtinIgnores = ["*~","~$*",".~lock.*","*.tmp","*.part",".DS_Store","Thumbs.db","desktop.ini",".sync_*.db",".sync_*.db-shm",".sync_*.db-wal","._sync_*.db",".owncloudsync.log*",".nextcloudsync.log*","*.~syncpart"];
  function addIgnore(p: string) { p = p.trim(); if (!p || ignore.includes(p)) return; ignore = [...ignore, p]; App.SetIgnorePatterns(ignore); }
  function addPat() { addIgnore(newPat); newPat = ""; }
  function rmPat(p: string) { ignore = ignore.filter(x => x !== p); App.SetIgnorePatterns(ignore); }

  // Flyout appearance (icon size / panel width / density / visible sections).
  let appearance = $state<{ dockIconSize: string; panelWidth: string; density: string; sections: string[] }>(
    { dockIconSize: "medium", panelWidth: "standard", density: "comfortable", sections: ["search", "activity", "storage"] });
  // Copy into a plain object — the Wails binding returns a class instance, which
  // Svelte 5 won't deep-proxy, so in-place edits (setIconSize etc.) wouldn't react.
  (async () => {
    const a = await App.FlyoutAppearance();
    appearance = { dockIconSize: a.dockIconSize, panelWidth: a.panelWidth, density: a.density, sections: [...(a.sections ?? [])] };
  })();
  const allSections = ["search", "activity", "storage"];
  const sectionLabels: Record<string, string> = { search: "Search bar", activity: "Recent activity", storage: "Storage bar" };
  function saveAppearance() { App.SetFlyoutAppearance($state.snapshot(appearance)); }
  function setIconSize(v: string) { appearance.dockIconSize = v; saveAppearance(); }
  function setWidth(v: string) { appearance.panelWidth = v; saveAppearance(); }
  function setDensity(v: string) { appearance.density = v; saveAppearance(); }
  function toggleSection(k: string) {
    appearance.sections = appearance.sections.includes(k)
      ? appearance.sections.filter(s => s !== k)
      : [...appearance.sections, k];
    saveAppearance();
  }
  // Drag-to-reorder the visible sections. We reorder live as the pointer passes
  // over a sibling row, then persist once on drop/end.
  let dragKey = $state<string | null>(null);
  function onSecDragStart(k: string) { dragKey = k; }
  function onSecDragEnter(k: string) {
    if (dragKey === null || dragKey === k) return;
    const list = [...appearance.sections];
    const from = list.indexOf(dragKey), to = list.indexOf(k);
    if (from === -1 || to === -1) return;
    list.splice(to, 0, list.splice(from, 1)[0]);
    appearance.sections = list;
  }
  function onSecDragEnd() { if (dragKey !== null) { dragKey = null; saveAppearance(); } }

  let allowed = $state<string[]>([]);
  let newAllowed = $state("");
  (async () => { allowed = (await App.GetAllowedFilenames()) ?? []; })();
  function addAllowed() { const p = newAllowed.trim(); if (!p || allowed.includes(p)) return; allowed = [...allowed, p]; newAllowed = ""; App.SetAllowedFilenames(allowed); }
  function rmAllowed(p: string) { allowed = allowed.filter(x => x !== p); App.SetAllowedFilenames(allowed); }

  // General
  let account = $state<{ signedIn: boolean; user: string; server: string }>({ signedIn: false, user: "", server: "" });
  (async () => { account = await App.AccountInfo(); })();
  let signOutOpen = $state(false);
  let clearOnSignOut = $state(false);
  async function signOut() {
    const err = await App.SignOut(clearOnSignOut);
    if (err) { alert(err); return; }
    signOutOpen = false;
    // Signing out may hand over to another configured account (multi-account);
    // re-query rather than assuming we're signed out.
    account = await App.AccountInfo();
    await loadAccounts();
  }

  // All configured accounts sync side by side; the active one is what the
  // windows and folder settings show. Switch, add, remove.
  let accounts = $state<{ id: string; user: string; server: string; active: boolean; status: string }[]>([]);
  let acctBusy = $state(false);
  async function loadAccounts() { accounts = (await App.ListAccounts()) ?? []; }
  loadAccounts();
  async function switchAccount(id: string) {
    acctBusy = true;
    const err = await App.SwitchAccount(id);
    acctBusy = false;
    if (err) { alert(err); return; }
    account = await App.AccountInfo();
    await loadAccounts();
  }
  async function removeAccount(id: string) {
    const err = await App.RemoveAccount(id);
    if (err) { alert(err); return; }
    await loadAccounts();
  }

  let autoSupported = $state(false), auto = $state(false);
  (async () => { autoSupported = await App.AutostartSupported(); auto = await App.AutostartEnabled(); })();
  async function toggleAuto() { auto = !auto; const err = await App.SetAutostart(auto); if (err) { auto = !auto; alert(err); } }

  let shellSupported = $state(false), shellOn = $state(false);
  (async () => { shellSupported = await App.ShellMenuSupported(); shellOn = await App.ShellMenuEnabled(); })();
  async function toggleShell() { shellOn = !shellOn; const err = await App.SetShellMenu(shellOn); if (err) { shellOn = !shellOn; alert(err); } }

  let navSupported = $state(false), navOn = $state(false);
  (async () => { navSupported = await App.SidebarSupported(); navOn = await App.SidebarEnabled(); })();
  async function toggleNav() { navOn = !navOn; const err = await App.SetSidebar(navOn); if (err) { navOn = !navOn; alert(err); } }

  let notifyOn = $state(true);
  (async () => { notifyOn = await App.NotificationsEnabled(); })();
  function toggleNotify() { notifyOn = !notifyOn; App.SetNotifications(notifyOn); }

  let dockOn = $state(true);
  (async () => { dockOn = await App.ShowAppDock(); })();
  function toggleDock() { dockOn = !dockOn; App.SetShowAppDock(dockOn); }

  let dockSide = $state("right");
  (async () => { dockSide = await App.AppDockSide(); })();
  function saveDockSide() { App.SetAppDockSide(dockSide); }

  let conflictPolicy = $state("ask");
  (async () => { conflictPolicy = await App.GetConflictPolicy(); })();
  function saveConflictPolicy() { App.SetConflictPolicy(conflictPolicy); }

  let theme = $state("system");
  (async () => { theme = await App.GetTheme(); })();
  function saveTheme() { App.SetTheme(theme); }

  let onDemandSupported = $state(false);
  let syncMode = $state("live");
  let syncModeBusy = $state(false);

  // "Available offline" browser over the virtual root's local placeholder tree.
  type OffEntry = { name: string; rel: string; pinned: boolean };
  let offCur = $state("");
  let offEntries = $state<OffEntry[]>([]);
  let pinBusy = $state(false);
  async function loadOffline() { offEntries = (await App.BrowseOffline(offCur)) ?? []; }
  const offNav = async (rel: string) => { offCur = rel; await loadOffline(); };
  const offUp = () => offNav(offCur.split("/").slice(0, -1).join("/"));
  async function togglePin(e: OffEntry) {
    pinBusy = true;
    const err = await App.SetOfflinePin(e.rel, !e.pinned);
    pinBusy = false;
    if (err) { alert(err); return; }
    await loadOffline();
  }
  $effect(() => { if (tab === "folders" && syncMode === "ondemand") loadOffline(); });
  (async () => { onDemandSupported = await App.OnDemandSupported(); syncMode = await App.GetSyncMode(); })();
  async function saveSyncMode() {
    syncModeBusy = true;
    const err = await App.SetSyncMode(syncMode);
    syncModeBusy = false;
    if (err) alert("File availability: " + err);
  }

  let logVerbose = $state(false);
  (async () => { logVerbose = await App.Verbose(); })();
  function toggleVerbose() { logVerbose = !logVerbose; App.SetVerbose(logVerbose); }

  // Connection health, refreshed while the Settings panel is open.
  let diag = $state(null);
  $effect(() => {
    const load = async () => { diag = await App.Diagnostics(); await loadAccounts(); };
    load();
    const id = setInterval(load, 3000);
    return () => clearInterval(id);
  });

  let lowMem = $state(true);
  (async () => { lowMem = await App.LowMemoryMode(); })();
  function toggleLowMem() { lowMem = !lowMem; App.SetLowMemoryMode(lowMem); }

  function minToTime(m: number) { return `${String(Math.floor(m / 60) % 24).padStart(2, "0")}:${String(m % 60).padStart(2, "0")}`; }
  function timeToMin(t: string) { const [h, mm] = t.split(":").map(Number); return (h || 0) * 60 + (mm || 0); }
  let schedEnabled = $state(false);
  let schedFrom = $state("22:00");
  let schedTo = $state("07:00");
  (async () => {
    const s = await App.GetPauseSchedule();
    schedEnabled = s.enabled;
    if (s.fromMin || s.toMin) { schedFrom = minToTime(s.fromMin); schedTo = minToTime(s.toMin); }
  })();
  function saveSched() { App.SetPauseSchedule(schedEnabled, timeToMin(schedFrom), timeToMin(schedTo)); }

  let version = $state("");
  (async () => { version = await App.Version(); })();

  // Business licence: paste a key to unlock business-tier features (central
  // policy, white-label …). Everything free today stays free without one.
  let lic = $state<{ hasLicence: boolean; licensed: boolean; customer: string; tier: string; seats: number; expires: string; expired: boolean; err: string }>(
    { hasLicence: false, licensed: false, customer: "", tier: "", seats: 0, expires: "", expired: false, err: "" });
  let licInput = $state("");
  let licBusy = $state(false);
  let licMsg = $state("");
  async function loadLicence() { lic = await App.LicenceInfo(); }
  loadLicence();
  async function activateLicence() {
    licBusy = true; licMsg = "";
    try {
      const err = await App.ActivateLicence(licInput);
      if (err) { licMsg = "Couldn't activate: " + err; }
      else { licMsg = "Licence activated."; licInput = ""; await loadLicence(); }
    } catch (e) { licMsg = "Couldn't activate: " + e; }
    finally { licBusy = false; }
  }
  async function removeLicence() {
    licBusy = true; licMsg = "";
    try { await App.RemoveLicence(); await loadLicence(); }
    finally { licBusy = false; }
  }

  // Report a problem: bundles logs + diagnostics into a zip (revealed in
  // Explorer) and opens the GitHub issue page — nothing is sent automatically.
  let reportBusy = $state(false);
  let reportMsg = $state("");
  async function reportProblem() {
    reportBusy = true; reportMsg = "Gathering logs…";
    try {
      const err = await App.ReportProblem();
      reportMsg = err ? "Couldn't create the report: " + err
        : "Report zip saved to Downloads — attach it to the GitHub issue that just opened.";
    } catch (e) {
      reportMsg = "Couldn't create the report: " + e;
    } finally {
      reportBusy = false;
    }
  }

  // Updates (GitHub releases check + in-app App Installer apply).
  let updateMsg = $state("");
  let updateURL = $state("");        // release page (fallback for loose builds)
  let updateNotes = $state("");      // release notes ("what's in this update")
  let updateAvail = $state(false);
  let updateBusy = $state(false);
  let canApply = $state(false);      // true only in the packaged install
  (async () => { canApply = await App.CanApplyUpdate(); })();
  // Pre-release channel. Only meaningful where we can self-update, so the UI
  // below is gated on canApply — which is false on the Store build (the Store
  // updates the app itself) and on loose dev builds.
  let beta = $state(false);
  (async () => { beta = await App.BetaUpdates(); })();
  async function toggleBeta() {
    beta = !beta;
    await App.SetBetaUpdates(beta);
    // Whatever the last check wrote described the channel we just left —
    // clear it rather than leave a stale verdict on screen.
    updateMsg = ""; updateURL = ""; updateNotes = ""; updateAvail = false;
  }
  async function checkUpdate() {
    updateBusy = true; updateMsg = "Checking…"; updateURL = ""; updateNotes = ""; updateAvail = false;
    const u = await App.CheckForUpdate();
    updateBusy = false;
    if (u.err) { updateMsg = "Couldn't check: " + u.err; return; }
    // Show the latest release's notes either way: as "what you'd get" when an
    // update is available, or "what's new in this version" when up to date.
    // Not when "ahead": u.notes there belongs to the older stable release,
    // and showing it under "you're on a newer build" reads as if that older
    // release is what the running build contains.
    updateNotes = u.ahead ? "" : u.notes;
    if (u.available) { updateMsg = "Update available: " + u.latest; updateURL = u.url; updateAvail = true; }
    else if (u.ahead) { updateMsg = "You're on a newer build than the current release" + (u.latest ? " (" + u.latest + ")" : ""); }
    else { updateMsg = "You're up to date" + (u.latest ? " (" + u.latest + ")" : ""); }
  }
  async function applyUpdate() {
    updateBusy = true; updateMsg = `Updating… ${brandName} will restart.`;
    const err = await App.ApplyUpdate();
    if (err) { updateBusy = false; updateMsg = "Update failed: " + err; }
    // On success the app is shut down by the installer and relaunched.
  }
</script>

<div class="win">
  <nav>
    <button class:active={tab==="folders"} onclick={() => tab="folders"}>Folders</button>
    <button class:active={tab==="sync"} onclick={() => tab="sync"}>Sync</button>
    <button class:active={tab==="exclusions"} onclick={() => tab="exclusions"}>Exclusions</button>
    <button class:active={tab==="appearance"} onclick={() => tab="appearance"}>Appearance</button>
    <button class:active={tab==="general"} onclick={() => tab="general"}>General</button>
  </nav>

  <div class="body">
    {#if policy.managed}
      <div class="mgbanner">🔒 Some settings are managed by your organisation.</div>
    {/if}
    {#if tab === "folders"}
      {#if onDemandSupported && (syncMode === "ondemand" || !adding)}
        <div class="field">
          <label>File availability</label>
          <p class="fhint">Live keeps every file on your disk. Virtual file system shows your whole account as online-only placeholders in your sync folder and downloads each file when you open it. Switching applies to the account and persists across restarts.</p>
          <select bind:value={syncMode} onchange={saveSyncMode} disabled={syncModeBusy || policy.lockSyncMode}>
            <option value="live">Live file system</option>
            <option value="ondemand">Virtual file system</option>
          </select>
          {#if policy.lockSyncMode}<p class="managed">🔒 Set by your organisation.</p>{/if}
        </div>
      {/if}
      {#if syncMode === "ondemand"}
        <!-- On-demand mode: the whole account is virtual; per-folder sync is off. -->
        <div class="row"><h3>Virtual file system</h3></div>
        <p class="fhint">Your whole Nextcloud account is available on demand in your sync folder — files stay online-only and download when you open them. Per-folder sync folders aren’t used in this mode. To sync individual folders to disk instead, switch <b>File availability</b> to <b>Live file system</b> above.</p>
        <div class="logrow"><button class="primary small" onclick={() => App.OpenSyncFolder()}>Open sync folder</button></div>

        <h3>Available offline</h3>
        <p class="fhint">Tick a folder to keep it fully on this PC — it downloads now and stays up to date for offline use. Untick to go back to online-only (already-downloaded files stay until you free them: right-click → <b>Free up space</b> in Explorer). Folders appear here as you browse them in Explorer.</p>
        <div class="crumb"><button onclick={offUp} disabled={!offCur}>⬆ Up</button><span>/{offCur}</span></div>
        {#if offEntries.length === 0}<p class="empty">(no folders here yet — open the sync folder and browse to populate it)</p>{/if}
        {#each offEntries as e}
          <div class="frow">
            <label class="cov"><input type="checkbox" checked={e.pinned} disabled={pinBusy} onchange={() => togglePin(e)} /> offline</label>
            <button class="name" onclick={() => offNav(e.rel)}>📁 {e.name}</button>
          </div>
        {/each}
      {:else if !adding && !managing}
        <!-- List view: existing synced folders -->
        <div class="row"><h3>Synced folders</h3><button class="primary small" onclick={startAdding}>＋ Add folder</button></div>
        {#if pairs.length === 0}
          <p class="empty">No folders synced yet. Click “Add folder” to choose one.</p>
        {:else}
          {#each pairs as p}
            <div class="conn">
              <div class="connmain">
                <span class="remote">/{trim(p.remoteRoot)}</span>
                <span class="arrow">→</span>
                <span class="local" title={p.localDir}>{p.localDir}</span>
              </div>
              <div class="connacts">
                <button class="link" title="Pick which subfolders sync; deselect to free space" onclick={() => startManage(p)}>Choose folders…</button>
                <button class="link" disabled={syncActive || moveBusy}
                        title={syncActive ? "Can’t move while a sync is running — wait for it to finish, then try again" : "Move this sync folder to a new location without re-downloading"}
                        onclick={() => movePair(p)}>Move…</button>
                <button class="link danger" title="Stop syncing this connection (local files are kept)" onclick={() => removePair(p)}>Remove</button>
              </div>
            </div>
          {/each}
        {/if}

      {:else if managing && managePair}
        <!-- Manage view: deselect/re-select subfolders of one connection -->
        <div class="row"><h3>Choose folders — /{trim(managePair.remoteRoot)}</h3><button class="link" onclick={exitManage}>← Done</button></div>
        <p class="hint">Untick a folder to stop syncing it. Your copy on the server is always kept — you'll be asked whether to also delete the files already downloaded here to free space. Re-tick to start syncing it again.</p>
        <div class="crumb">
          <button onclick={mUp} disabled={trim(mCur) === trim(managePair.remoteRoot)}>⬆ Up</button>
          <span>/{mCur}</span>
        </div>
        {#if mEntries.length === 0}<p class="empty">(empty)</p>{/if}
        {#each mEntries as e}
          <div class="frow">
            {#if e.isDir}
              <input class="selbox" type="checkbox" checked={mChecked(e.path)} onchange={() => onToggleManage(e)} />
              <button class="name" onclick={() => mNavigate(e.path)}>📁 {e.name}</button>
            {:else}
              <span class="selspace"></span>
              <span class="name file">📄 {e.name}</span>
            {/if}
          </div>
        {/each}
        {#if addErr}<pre class="err">{addErr}</pre>{/if}

        {#if pending}
          <div class="modalback">
            <div class="addpanel modalbox">
              <div class="addhead">Stop syncing “{pending.name}”?</div>
              <p class="hint">Your copy on the server is kept. What about the files already downloaded to this computer?</p>
              <div class="addbtns">
                <button class="primary" onclick={() => confirmDeselect(false)}>Keep files here</button>
                <button class="danger" onclick={() => confirmDeselect(true)}>Delete to free space</button>
              </div>
              <button class="link" onclick={() => (pending = null)}>Cancel</button>
            </div>
          </div>
        {/if}

      {:else if step === "dest" && everything}
        <!-- Whole-account: one local folder mirrors the entire Nextcloud. -->
        <div class="row"><h3>Sync your entire Nextcloud</h3><button class="link" onclick={() => (step = "browse")}>← Back</button></div>
        <div class="addpanel">
          <div class="addhead">Local folder to mirror your whole account into:</div>
          <div class="addrow2">
            <input bind:value={parent} placeholder="e.g. C:\Users\You\Nextcloud" />
            <button onclick={browseParent}>Browse…</button>
          </div>
          <p class="hint">Everything in your Nextcloud syncs here — you can exclude individual subfolders afterwards from the folder list. Big dev folders like <b>node_modules</b> and <b>.git</b> sync too; exclude them under Exclusions for faster syncing (<b>.git</b> is best excluded if you work on a repo across machines).</p>
          {#if addErr}<pre class="err">{addErr}</pre>{/if}
          <div class="addbtns">
            <button class="primary" onclick={confirmAdd} disabled={busy || !parent.trim()}>{busy ? "Setting up…" : "Sync everything here"}</button>
            <button onclick={exitAdding}>Cancel</button>
          </div>
        </div>

      {:else if step === "dest"}
        <!-- Step 2: one local parent folder; each picked folder becomes a subfolder -->
        <div class="row"><h3>Choose where to sync</h3><button class="link" onclick={() => (step = "browse")}>← Back</button></div>
        <div class="addpanel">
          <div class="addhead">Local parent folder (on this computer):</div>
          <div class="addrow2">
            <input bind:value={parent} placeholder="e.g. C:\Users\You\Nextcloud" />
            <button onclick={browseParent}>Browse…</button>
          </div>
          <p class="hint">Each Nextcloud folder below syncs into a subfolder of that parent. Final path = parent \ subfolder name.</p>
          {#if selected.some(r => coveringPair(r))}
            <p class="hint warnhint">Some of these are already synced elsewhere — they'll <b>move</b> to this location: the old local copy is removed and re-downloaded here. Your server copy is untouched.</p>
          {/if}
          <div class="destlist">
            <div class="destrow desthead">
              <span class="remote">Nextcloud folder</span>
              <span class="arrow"></span>
              <span class="namehdr">Local subfolder name</span>
            </div>
            {#each selected as r}
              <div class="destrow">
                <span class="remote" title={"/" + r}>/{r}</span>
                <span class="arrow">→</span>
                <input class="nameinput" bind:value={names[r]} />
              </div>
            {/each}
          </div>
          {#if addErr}<pre class="err">{addErr}</pre>{/if}
          <div class="addbtns">
            <button class="primary" onclick={confirmAdd} disabled={busy}>{busy ? "Adding…" : `Add ${selected.length} folder${selected.length > 1 ? "s" : ""}`}</button>
            <button onclick={exitAdding}>Cancel</button>
          </div>
        </div>

      {:else}
        <!-- Step 1: browse Nextcloud and tick folders to sync -->
        <div class="row"><h3>{cur === "" && pairs.length === 0 ? "How do you want to sync?" : "Pick folders to sync"}</h3><button class="link" onclick={exitAdding}>← Back</button></div>
        {#if cur === "" && pairs.length === 0}
          <button class="everything" onclick={chooseEverything}>
            <span class="etop"><span class="etitle">☁ Sync my entire Nextcloud into one folder</span><span class="ebadge">Recommended</span></span>
            <span class="ehint">Mirror the whole account into one folder — the simplest setup. You can exclude individual folders later.</span>
          </button>
          <div class="orsep">— or tick individual folders below to sync only those —</div>
        {/if}
        <div class="crumb"><button onclick={up} disabled={!cur}>⬆ Up</button><span>/{cur}</span></div>
        {#if entries.length === 0}<p class="empty">(empty)</p>{/if}
        {#each entries as e}
          <div class="frow">
            {#if e.isDir}
              {@const st = folderState(e.path)}
              {#if st === "synced"}
                <!-- Already its own sync connection — manage or move it from the list. -->
                <span class="selspace"></span>
                <button class="name" onclick={() => navigate(e.path)}>📁 {e.name}</button>
                <span class="tag">✓ its own sync</span>
              {:else}
                <!-- Selectable for a NEW group. "covered" folders (already in a broader
                     sync, e.g. the whole-account one) can be ticked to move here. -->
                <input class="selbox" type="checkbox" checked={selected.includes(trim(e.path))} onchange={() => toggleSelect(e.path)} />
                <button class="name" onclick={() => navigate(e.path)}>📁 {e.name}</button>
                {#if st === "covered"}
                  {@const cov = coveringPair(e.path)}
                  <span class="tag moving" title={cov ? "Currently synced in " + cov.localDir : ""}>in {cov ? localName(cov.localDir) : "another sync"}{selected.includes(trim(e.path)) ? " → moves here" : ""}</span>
                {/if}
              {/if}
            {:else}
              <span class="selspace"></span>
              <span class="name file">📄 {e.name}</span>
            {/if}
          </div>
        {/each}
        <div class="addbar">
          <span>{selected.length} selected</span>
          <button class="primary small" disabled={!selected.length} onclick={toDest}>Next →</button>
        </div>
      {/if}

    {:else if tab === "sync"}
      <h3>Bandwidth</h3>
      <div class="field"><label>Upload limit (KiB/s, 0 = unlimited)</label><input type="number" bind:value={up_} onchange={saveLimits} disabled={policy.lockBandwidth} /></div>
      <div class="field"><label>Download limit (KiB/s, 0 = unlimited)</label><input type="number" bind:value={down_} onchange={saveLimits} disabled={policy.lockBandwidth} /></div>
      {#if policy.lockBandwidth}<p class="managed">🔒 Set by your organisation.</p>{:else if savedFlash === "limits"}<span class="saved">Saved ✓</span>{/if}

      <h3>Quiet hours</h3>
      <label class="check"><input type="checkbox" bind:checked={schedEnabled} onchange={saveSched} /> Auto-pause syncing during quiet hours</label>
      {#if schedEnabled}
        <div class="schedrow">From <input type="time" bind:value={schedFrom} onchange={saveSched} /> to <input type="time" bind:value={schedTo} onchange={saveSched} /></div>
      {/if}

      <h3>Conflicts</h3>
      <div class="field">
        <label>When a file changes in both places</label>
        <p class="fhint">How {brandName} resolves sync conflicts by default.</p>
        <select bind:value={conflictPolicy} onchange={saveConflictPolicy}>
          <option value="ask">Ask me each time</option>
          <option value="keepboth">Keep both versions</option>
          <option value="newest">Keep the newest</option>
        </select>
      </div>

      <h3>Performance</h3>
      <label class="check"><input type="checkbox" checked={lowMem} onchange={toggleLowMem} /> Low memory mode</label>
      <p class="fhint">Keeps {brandName}'s footprint small by reading sync state from disk instead of holding it all in memory. Recommended. Turn off only if you want the fastest possible background re-syncs on a very large account (uses noticeably more RAM); local edits are unaffected either way.</p>

    {:else if tab === "exclusions"}
      <h3>Ignore patterns</h3>
      <p class="fhint">Glob patterns excluded from every sync folder (left untouched on both sides — not synced, not deleted). A name with no slash matches at any depth (<code>node_modules</code>, <code>*.log</code>); a name with a slash matches a full path (<code>build/out</code>). Applies on the next sync.</p>
      <div class="addrow"><input placeholder="*.log or node_modules/" bind:value={newPat} onkeydown={(e) => e.key === "Enter" && addPat()} /><button onclick={addPat}>Add</button></div>
      {#each ignore as p}<div class="irow"><span>{p}</span><button class="link" onclick={() => rmPat(p)}>Remove</button></div>{/each}
      {#if ignore.length === 0}<p class="empty">No custom patterns yet.</p>{/if}
      <p class="fhint sugg">💡 Big dev folders sync by default. Exclude them for faster syncing — and <b>.git</b> is best excluded if you work on a repo across machines (file-syncing a live <code>.git</code> can corrupt it):
        {#each ["node_modules", ".git", ".svn", ".hg"] as q}{#if !ignore.includes(q)}<button class="chipadd" onclick={() => addIgnore(q)}>+ {q}</button>{/if}{/each}
      </p>
      <details class="builtins">
        <summary>Always skipped (built-in)</summary>
        <p class="fhint">Temp/lock files, OS cruft, and other sync clients' journal files — never user data:</p>
        <div class="binfo">{#each builtinIgnores as b}<code>{b}</code>{/each}</div>
      </details>

      <h3>Allowed filenames</h3>
      <p class="fhint">{brandName} normally blocks web-server config files — <b>.htaccess</b>, <b>.htpasswd</b>, <b>.user.ini</b> — because most Nextcloud servers reject them. Add any you want to sync anyway; this only works if your server actually accepts them. Takes effect after restarting {brandName}.</p>
      <div class="addrow"><input placeholder=".htaccess" bind:value={newAllowed} onkeydown={(e) => e.key === "Enter" && addAllowed()} /><button onclick={addAllowed}>Add</button></div>
      {#each allowed as p}<div class="irow"><span>{p}</span><button class="link" onclick={() => rmAllowed(p)}>Remove</button></div>{/each}
      {#if allowed.length === 0}<p class="empty">None — the standard blocks apply.</p>{/if}

    {:else if tab === "appearance"}
      <h3>Panel</h3>
      <p class="fhint">Customise the tray flyout panel — changes apply live.</p>
      <div class="approw"><label>Width</label>
        <div class="seg">
          {#each [["compact","Compact"],["standard","Standard"],["wide","Wide"]] as opt}
            <button class:sel={appearance.panelWidth===opt[0]} onclick={() => setWidth(opt[0])}>{opt[1]}</button>
          {/each}
        </div>
      </div>
      <div class="approw"><label>Spacing</label>
        <div class="seg">
          {#each [["comfortable","Comfortable"],["compact","Compact"]] as opt}
            <button class:sel={appearance.density===opt[0]} onclick={() => setDensity(opt[0])}>{opt[1]}</button>
          {/each}
        </div>
      </div>

      <h3>App dock</h3>
      <label class="check"><input type="checkbox" checked={dockOn} onchange={toggleDock} /> Show the app dock (a strip of your pinned apps along an edge of the menu)</label>
      {#if dockOn}
        <div class="approw"><label>Icon size</label>
          <div class="seg">
            {#each [["small","Small"],["medium","Medium"],["large","Large"]] as opt}
              <button class:sel={appearance.dockIconSize===opt[0]} onclick={() => setIconSize(opt[0])}>{opt[1]}</button>
            {/each}
          </div>
        </div>
        <div class="approw"><label>Position</label>
          <select class="possel" bind:value={dockSide} onchange={saveDockSide}>
            <option value="right">Right edge</option>
            <option value="left">Left edge</option>
            <option value="bottom">Bottom strip</option>
          </select>
        </div>
      {/if}

      <h3>Sections</h3>
      <p class="fhint">Drag to reorder; hide the ones you don't need. Applies live.</p>
      <div class="seclist" role="list">
        {#each appearance.sections as k (k)}
          <div class="secrow" class:drag={dragKey === k} role="listitem" draggable="true"
               ondragstart={() => onSecDragStart(k)}
               ondragenter={() => onSecDragEnter(k)}
               ondragover={(e) => e.preventDefault()}
               ondragend={onSecDragEnd}>
            <span class="sechandle" title="Drag to reorder">⠿</span>
            <span class="secname">{sectionLabels[k]}</span>
            <button class="link" onclick={() => toggleSection(k)}>Hide</button>
          </div>
        {/each}
        {#if appearance.sections.length === 0}
          <p class="empty">All sections hidden — the flyout shows just the header and dock.</p>
        {/if}
      </div>
      {#each allSections.filter(k => !appearance.sections.includes(k)) as k}
        <button class="link secadd" onclick={() => toggleSection(k)}>+ Show {sectionLabels[k]}</button>
      {/each}

    {:else}
      <h3>Account</h3>
      {#if account.signedIn}
        <div class="acct">
          <div class="acctinfo"><b>{account.user}</b><span>{account.server}</span></div>
          {#if policy.allowSignOut}<button class="signout" onclick={() => (signOutOpen = !signOutOpen)}>Sign out</button>{:else}<span class="managed">🔒 Managed by your organisation</span>{/if}
        </div>
        {#if signOutOpen}
          <div class="signoutbox">
            <label class="check"><input type="checkbox" bind:checked={clearOnSignOut} /> Also remove this device's sync setup &amp; database</label>
            <p class="fhint">Leave unticked for a temporary sign-out — your synced folders are remembered for a quick re-login. Tick it for a clean reset: recommended if you'll delete the local sync folder, since it stops a fresh login from mistaking the missing files for deletions and removing them on the server. Your preferences are kept either way.</p>
            <div class="addbtns">
              <button class="signout" onclick={signOut}>{clearOnSignOut ? "Sign out & clear data" : "Sign out"}</button>
              <button onclick={() => (signOutOpen = false)}>Cancel</button>
            </div>
          </div>
        {/if}
      {:else}
        <p class="empty">Signed out — use the sign-in window to connect an account.</p>
      {/if}
      {#if accounts.length > 1}
        <p class="fhint">All accounts sync at the same time. The <b>shown</b> account is the one the menus, folder settings, and search use — switch to manage a different one.</p>
        {#each accounts as ac}
          <div class="conn">
            <div class="connmain">
              <span class="remote">{ac.user}</span>
              <span class="local" title={ac.server}>{ac.server}</span>
              {#if ac.active}<span class="tag">✓ shown</span>{/if}
            </div>
            <div class="connacts">
              {#if ac.status && !ac.active}<span class="acctstatus" title={ac.status}>{ac.status}</span>{/if}
              {#if !ac.active}
                <button class="link" disabled={acctBusy} onclick={() => switchAccount(ac.id)}>Show</button>
                <button class="link danger" title="Remove this account from this device (its local files are kept)" onclick={() => removeAccount(ac.id)}>Remove</button>
              {/if}
            </div>
          </div>
        {/each}
      {/if}
      <div class="logrow"><button class="primary small" onclick={() => App.AddAccount()}>＋ Add account</button></div>

      <h3>Preferences</h3>
      <div class="field">
        <label>Default sync location</label>
        <p class="fhint">Where newly-added folders are placed by default.</p>
        <div class="baserow"><input bind:value={baseDir} onchange={saveBase} />{#if savedFlash === "base"}<span class="saved">Saved ✓</span>{/if}</div>
      </div>

      <div class="field">
        <label>Appearance</label>
        <select bind:value={theme} onchange={saveTheme}>
          <option value="system">Match Nextcloud theme</option>
          <option value="light">Light</option>
          <option value="dark">Dark</option>
        </select>
      </div>
      <label class="check"><input type="checkbox" checked={notifyOn} onchange={toggleNotify} /> Show desktop notifications</label>

      <h3>System integration</h3>
      {#if autoSupported}
        <label class="check"><input type="checkbox" checked={auto} onchange={toggleAuto} /> Start {brandName} when I log in</label>
      {:else}
        <p class="empty">Startup options aren't available on this platform.</p>
      {/if}
      {#if shellSupported}
        <label class="check"><input type="checkbox" checked={shellOn} onchange={toggleShell} /> Add “Share with {brandName}” to the Explorer right-click menu</label>
      {/if}
      {#if navSupported}
        <label class="check"><input type="checkbox" checked={navOn} onchange={toggleNav} /> Show {brandName} in the Explorer sidebar (points at your default sync location)</label>
      {/if}

      <h3>Troubleshooting</h3>
      <label class="check"><input type="checkbox" checked={logVerbose} onchange={toggleVerbose} /> Verbose (debug) logging</label>
      <h4 class="dhealth">Connection health</h4>
      {#if diag}
        <div class="diaggrid">
          <span class="dk">Server</span>
          <span class="dv">{diag.serverURL || "—"}{#if diag.serverVersion} · Nextcloud {diag.serverVersion}{/if}</span>
          <span class="dk">Account</span>
          <span class="dv">{diag.account || "—"}</span>
          <span class="dk">Real-time push</span>
          <span class="dv">
            {#if !diag.pushAvailable}<span class="dmuted">not available on this server</span>
            {:else if diag.pushConnected}<span class="dok">● connected</span>{#if diag.pushUptime} · up {diag.pushUptime}{/if}
            {:else}<span class="dbad">● reconnecting…</span>{/if}
          </span>
          <span class="dk">Last sync</span>
          <span class="dv">{diag.lastSync}{#if diag.lastStatus} · {diag.lastStatus}{/if}</span>
        </div>
      {/if}

      <div class="logrow">
        <button class="primary small" onclick={() => App.OpenLogs()}>View logs</button>
        <button class="small reportbtn" disabled={reportBusy} onclick={reportProblem}>Report a problem…</button>
        {#if reportMsg}<span class="fhint">{reportMsg}</span>{/if}
      </div>

      <h3>About</h3>
      <div class="updaterow">
        <span class="about">{brandName} {version}</span>
        <button class="link" onclick={checkUpdate} disabled={updateBusy}>Check for updates</button>
        {#if updateMsg}<span class="upmsg">{updateMsg}</span>{/if}
        {#if updateAvail}
          {#if canApply}
            <button class="link" onclick={applyUpdate} disabled={updateBusy}>Update now</button>
          {:else if updateURL}
            <button class="link" onclick={() => App.OpenURL(updateURL)}>Get update</button>
          {/if}
        {/if}
      </div>
      {#if canApply}
        <label class="check"><input type="checkbox" checked={beta} onchange={toggleBeta} /> Get beta releases early</label>
        <p class="fhint">Beta builds reach you before everyone else and are less tested. Turning this off stops future betas — it won't move you back, so you'll stay on your current build until a normal release overtakes it.</p>
      {/if}
      {#if updateNotes}
        <div class="relnotes">{updateNotes}</div>
      {/if}
      <h3>Licence</h3>
      {#if lic.licensed}
        <p class="fhint">
          <b>Business licence</b> — {lic.customer}{lic.seats ? ` · ${lic.seats} seats` : ""}{lic.expires ? ` · expires ${lic.expires}` : " · perpetual"}.
        </p>
        <div class="logrow"><button class="small reportbtn" disabled={licBusy} onclick={removeLicence}>Remove licence</button></div>
      {:else}
        <p class="fhint">
          Nimbo is <b>free for personal use</b>. A business licence unlocks central deployment policy and white-label branding for organisations — <button class="link" onclick={() => App.OpenURL(brandWebsite + "/business.html")}>about business licensing</button>.
          {#if lic.hasLicence && lic.err}<br /><span style="color:#c0392b">Installed licence: {lic.err}.</span>{/if}
        </p>
        <div class="addrow">
          <input placeholder="NIMBO-LIC-1.…" bind:value={licInput} onkeydown={(e) => e.key === "Enter" && activateLicence()} />
          <button disabled={licBusy} onclick={activateLicence}>Activate</button>
        </div>
      {/if}
      {#if licMsg}<p class="fhint">{licMsg}</p>{/if}

      <p class="aboutco">
        © {new Date().getFullYear()} {brandCompany}
        · <button class="link" onclick={() => App.OpenURL(brandWebsite)}>{hostname(brandWebsite)}</button>
        · <button class="link" onclick={() => App.OpenURL("mailto:" + brandSupport)}>{brandSupport}</button>
      </p>
    {/if}
  </div>
</div>

<style>
  .win { height: 100%; display: flex; flex-direction: column; background: var(--bg); color: var(--fg); }
  nav { display: flex; border-bottom: 1px solid var(--border); }
  nav button { flex: 1; padding: 12px 8px; border: none; background: none; cursor: pointer; font-size: 13px;
               color: var(--fg2); border-bottom: 2px solid transparent; }
  nav button:hover { background: var(--panel-2); }
  nav button.active { color: var(--accent); border-bottom-color: var(--accent); font-weight: 600; }
  .body { flex: 1; overflow-y: auto; padding: 14px 16px; }
  .hint { color: var(--fg2); font-size: 12px; margin: 0 0 12px; }
  .warnhint { color: #8a6d1a; background: #fff8e6; border: 1px solid #f5e3b0; border-radius: 6px; padding: 7px 9px; }
  .approw { display: flex; align-items: center; justify-content: space-between; gap: 12px; margin: 12px 0; }
  .approw label { font-size: 13px; color: var(--fg); }
  .seg { display: inline-flex; border: 1px solid var(--border-2); border-radius: 8px; overflow: hidden; }
  .seg button { padding: 6px 12px; border: none; border-left: 1px solid var(--border-2); background: var(--bg); color: var(--fg); cursor: pointer; font-size: 12px; }
  .seg button:first-child { border-left: none; }
  .seg button.sel { background: var(--accent); color: #fff; }
  .seg button:not(.sel):hover { background: var(--hover); }
  .fhint { color: var(--muted); font-size: 12px; margin: 0 0 6px; }
  .fhint code { font-size: 11px; background: var(--bg2, rgba(127,127,127,.12)); padding: 0 3px; border-radius: 3px; }
  .flabel { display: block; font-size: 13px; font-weight: 600; margin: 12px 0 4px; }
  .dhealth { font-size: 13px; font-weight: 600; margin: 16px 0 6px; }
  .diaggrid { display: grid; grid-template-columns: auto 1fr; gap: 4px 12px; font-size: 12px; margin-bottom: 6px; }
  .dk { color: var(--muted); white-space: nowrap; }
  .dv { color: var(--fg); word-break: break-word; }
  .dok { color: #2e9e44; }
  .dbad { color: #d08b00; }
  /* Flyout section reorder list */
  .seclist { display: flex; flex-direction: column; gap: 6px; margin-bottom: 8px; }
  .secrow { display: flex; align-items: center; gap: 10px; padding: 8px 10px; border: 1px solid var(--border-2);
            border-radius: 8px; background: var(--panel); cursor: grab; }
  .secrow.drag { opacity: .5; border-color: var(--accent); background: var(--tint, var(--hover)); }
  .sechandle { flex: 0 0 auto; color: var(--muted); font-size: 14px; line-height: 1; cursor: grab; user-select: none; }
  .secname { flex: 1; min-width: 0; font-size: 13px; color: var(--fg); }
  .secadd { display: inline-block; margin: 0 10px 6px 0; }
  .dmuted { color: var(--muted); }
  .baserow { display: flex; gap: 8px; align-items: center; margin-bottom: 12px; }
  .baserow label { font-size: 13px; white-space: nowrap; }
  .baserow input { flex: 1; padding: 6px 8px; border: 1px solid var(--border-2); border-radius: 5px; background: var(--bg); color: var(--fg); }
  .everything { display: flex; flex-direction: column; gap: 3px; width: 100%; text-align: left; cursor: pointer;
                padding: 14px 15px; border: 2px solid var(--accent); border-radius: 10px;
                background: color-mix(in srgb, var(--accent) 12%, var(--bg)); color: var(--fg); }
  .everything:hover { background: color-mix(in srgb, var(--accent) 20%, var(--bg)); }
  .everything .etop { display: flex; align-items: center; justify-content: space-between; gap: 8px; }
  .everything .etitle { font-size: 14.5px; font-weight: 700; }
  .everything .ebadge { flex: 0 0 auto; font-size: 10px; font-weight: 700; text-transform: uppercase; letter-spacing: .04em;
                        background: var(--accent); color: #fff; border-radius: 999px; padding: 2px 8px; }
  .everything .ehint { font-size: 12px; color: var(--fg2); }
  .orsep { text-align: center; color: var(--muted); font-size: 12px; margin: 12px 0; }
  .sugg .chipadd { margin-left: 6px; padding: 2px 8px; border: 1px solid var(--border-2); border-radius: 999px;
                   background: var(--bg); color: var(--accent); cursor: pointer; font-size: 12px; }
  .sugg .chipadd:hover { background: var(--tint); border-color: var(--accent); }
  .builtins { margin: 8px 0 4px; }
  .builtins summary { cursor: pointer; color: var(--fg2); font-size: 12.5px; }
  .builtins .binfo { display: flex; flex-wrap: wrap; gap: 5px; margin-top: 6px; }
  .builtins .binfo code { font-size: 11px; background: var(--panel-2); border: 1px solid var(--border-2); border-radius: 4px; padding: 1px 5px; }
  .crumb { display: flex; gap: 10px; align-items: center; margin-bottom: 8px; color: var(--fg2); font-size: 13px; }
  h3 { font-size: 12px; text-transform: uppercase; color: var(--muted); margin: 18px 0 8px; }
  h3:first-of-type { margin-top: 0; }
  /* Section header: heading left, action (Add folder / Back / Done) right, with
     clear air before the content below it. */
  .row { display: flex; align-items: center; justify-content: space-between; gap: 10px; margin: 18px 0 12px; }
  .row:first-child { margin-top: 0; }
  .row h3 { margin: 0; }
  .conn { display: flex; align-items: center; justify-content: space-between; gap: 10px; padding: 8px 10px;
          border: 1px solid var(--border); border-radius: 8px; margin-bottom: 6px; background: var(--panel); }
  .connmain { display: flex; align-items: center; gap: 8px; min-width: 0; font-size: 13px; }
  .connmain .remote { font-weight: 600; color: var(--accent); white-space: nowrap; }
  .connmain .arrow { color: var(--muted); }
  .acctstatus { color: var(--muted); font-size: 11px; max-width: 150px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .saved { color: #2e9e5b; font-size: 12px; font-weight: 600; align-self: center; animation: savedfade 1.5s forwards; }
  .managed { color: var(--muted); font-size: 12px; margin: 4px 0 0; }
  .mgbanner { background: color-mix(in srgb, var(--accent) 12%, var(--panel)); border: 1px solid var(--accent);
              border-radius: 8px; padding: 8px 12px; font-size: 13px; color: var(--fg); margin-bottom: 14px; }
  @keyframes savedfade { 0%, 70% { opacity: 1; } 100% { opacity: 0; } }
  .relnotes { white-space: pre-line; font-size: 12px; color: var(--fg2); background: var(--panel);
              border: 1px solid var(--border); border-radius: 6px; padding: 8px 10px; margin-top: 6px;
              max-height: 140px; overflow-y: auto; }
  .reportbtn { padding: 6px 12px; border: 1px solid var(--border-2); border-radius: 5px;
               background: var(--bg); color: var(--fg); cursor: pointer; font-size: 12px; }
  .aboutco { color: var(--muted); font-size: 12px; margin: 10px 0 0; }
  .aboutco .link { font-size: 12px; }
  .connmain .local { color: var(--fg2); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .danger { color: #c0392b; }
  .connacts { display: flex; align-items: center; gap: 10px; flex: 0 0 auto; }
  .modalback { position: fixed; inset: 0; background: rgba(0,0,0,.45); display: flex;
               align-items: center; justify-content: center; z-index: 50; }
  .modalbox { max-width: 360px; width: 90%; margin: 0; box-shadow: 0 10px 40px rgba(0,0,0,.4); }
  .addpanel { border: 1px solid var(--border); background: var(--panel); border-radius: 8px; padding: 12px; margin-bottom: 12px; }
  .addhead { font-size: 13px; margin-bottom: 8px; }
  .addrow2 { display: flex; gap: 8px; }
  .addrow2 input { flex: 1; padding: 7px 8px; border: 1px solid var(--border-2); border-radius: 5px; background: var(--bg); color: var(--fg); }
  .addrow2 button { padding: 7px 12px; border: 1px solid var(--border-2); border-radius: 5px; background: var(--bg); color: var(--fg); cursor: pointer; }
  .addbtns { display: flex; gap: 8px; margin-top: 10px; }
  .addbtns .primary { padding: 7px 14px; }
  .addbtns button:not(.primary) { padding: 7px 12px; border: 1px solid var(--border-2); border-radius: 6px; background: var(--bg); color: var(--fg); cursor: pointer; }
  .err { color: #e06b6b; font-size: 12px; margin: 8px 0 0; white-space: pre-wrap; font-family: inherit; }
  .frow { display: flex; align-items: center; justify-content: space-between; gap: 8px; padding: 4px 0; }
  .name { background: none; border: none; cursor: pointer; font-size: 13px; color: var(--fg); text-align: left; flex: 1; min-width: 0;
          overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .name:hover { color: var(--accent); }
  .name.file { cursor: default; color: var(--fg2); }
  .acts { display: flex; align-items: center; gap: 10px; flex: 0 0 auto; }
  .tag { font-size: 12px; color: #2e7d32; white-space: nowrap; flex: 0 0 auto; }
  .tag.moving { color: #b26a00; }
  .selbox { flex: 0 0 auto; margin: 0; }
  .selspace { flex: 0 0 auto; width: 13px; }
  .addbar { display: flex; align-items: center; justify-content: space-between; gap: 10px;
            margin-top: 14px; padding-top: 10px; border-top: 1px solid var(--border); font-size: 13px; color: var(--fg2); }
  .destlist { margin-top: 8px; display: flex; flex-direction: column; gap: 6px; }
  .destrow { display: flex; align-items: center; gap: 8px; }
  .destrow .remote { font-weight: 600; color: var(--accent); max-width: 45%; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .destrow .arrow { color: var(--muted); }
  .nameinput { flex: 1; padding: 5px 7px; border: 1px solid var(--border-2); border-radius: 5px; font-size: 12.5px; background: var(--bg); color: var(--fg); }
  .namehdr { flex: 1; }
  .desthead { margin-bottom: 2px; }
  .desthead .remote, .namehdr { font-size: 11px; text-transform: uppercase; letter-spacing: 0.3px; color: var(--muted); font-weight: 600; }
  .cov { display: flex; align-items: center; gap: 5px; font-size: 12px; color: var(--fg2); white-space: nowrap; }
  .field { display: flex; flex-direction: column; gap: 4px; margin-bottom: 12px; max-width: 340px; }
  .field label { font-size: 12px; color: var(--fg2); }
  .field input { padding: 7px 8px; border: 1px solid var(--border-2); border-radius: 5px; background: var(--bg); color: var(--fg); }
  .field select { padding: 7px 8px; border: 1px solid var(--border-2); border-radius: 5px; font-size: 13px; max-width: 240px; background: var(--bg); color: var(--fg); }
  .addrow { display: flex; gap: 8px; margin-bottom: 12px; }
  .addrow input { flex: 1; padding: 7px 8px; border: 1px solid var(--border-2); border-radius: 5px; background: var(--bg); color: var(--fg); }
  .irow { display: flex; justify-content: space-between; padding: 6px 0; border-bottom: 1px solid var(--border-soft); font-size: 13px; }
  .check { display: flex; gap: 8px; align-items: center; font-size: 13px; margin-bottom: 12px; }
  .possel { padding: 7px 8px; border: 1px solid var(--border-2); border-radius: 5px; font-size: 13px; max-width: 200px; background: var(--bg); color: var(--fg); }
  button { font-size: 13px; }
  .primary { padding: 8px 14px; border: none; border-radius: 6px; background: var(--accent); color: #fff; cursor: pointer; }
  .primary:hover { background: var(--accent-dark); }
  .primary.small { padding: 6px 12px; font-size: 12.5px; }
  .link { background: none; border: none; color: var(--accent); cursor: pointer; }
  .link:disabled { opacity: .45; cursor: not-allowed; }
  .empty { color: var(--muted); font-size: 13px; }
  .logrow { margin: 4px 0 4px; }
  .schedrow { display: flex; align-items: center; gap: 8px; font-size: 13px; color: var(--fg2); margin: 0 0 12px 24px; }
  .schedrow input { padding: 5px 7px; border: 1px solid var(--border-2); border-radius: 5px; background: var(--bg); color: var(--fg); }
  .about { color: var(--muted); font-size: 12px; }
  .updaterow { margin-top: 20px; display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
  .upmsg { color: var(--fg2); font-size: 12px; }
  .acct { display: flex; align-items: center; justify-content: space-between; gap: 10px; padding: 10px 12px;
          border: 1px solid var(--border); border-radius: 8px; background: var(--panel); margin-bottom: 16px; }
  .acctinfo { display: flex; flex-direction: column; min-width: 0; }
  .acctinfo b { font-size: 14px; }
  .acctinfo span { font-size: 12px; color: var(--muted); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .signout { flex: 0 0 auto; padding: 7px 14px; border: 1px solid var(--border-2); border-radius: 6px;
             background: var(--bg); color: #c0392b; cursor: pointer; font-size: 13px; }
  .signout:hover { background: #fdeceb; }
  .signoutbox { margin: 8px 0 12px; padding: 12px; border: 1px solid var(--border); border-radius: 8px; background: var(--panel); }
  .signoutbox .addbtns { margin-top: 8px; }
</style>
