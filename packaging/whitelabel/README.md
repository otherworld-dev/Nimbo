# White-label builds

**Internal / commercial.** How to produce a branded build of the app for a
hosting provider or partner ("AcmeCloud Sync" instead of "Nimbo"). White-label
distribution is a **commercial use** under the PolyForm Noncommercial licence —
it requires an agreement with Otherworld Dev Ltd.

A branded build is a **config + asset swap, then rebuild** — no source changes.

## One command: `build-partner.ps1`

Drop a partner profile under `packaging/whitelabel/partners/<slug>/`:

- `brand.json` — the embedded brand (becomes `internal/brand/brand.json`).
- `partner.json` — `{ identityName, publisher, displayName, publisherDisplayName, feedBaseUrl }`.
  `publisher` is the partner's code-signing cert subject (in `Cert:\CurrentUser\My`).

Then:

    packaging\whitelabel\build-partner.ps1 -Partner <slug>

It swaps in the brand, regenerates the icons from the accent, patches the MSIX
manifest identity, builds + signs with their cert, writes the `.appinstaller`, and
drops `<Name>.msix` + `<Name>.appinstaller` into `partners/<slug>/dist/` (gitignored)
— then restores the stock source files via `git checkout`, so a stock release is
never left with partner branding. Needs a clean working tree for the stock files it
touches. See `partners/example/` for a template. (The offline Setup.exe isn't
automated yet — that's still the manual `build-exe-installer.ps1` path.)

The manual steps below are what it does under the hood (useful for a one-off or to
debug a single step).

## What a brand controls

`internal/brand/brand.json` is embedded at build time and drives every
user-visible identity value:

| Field | Used for |
|---|---|
| `name` | product name throughout the app (titles, flyout, About, toasts, sync-root display name, the "<name> - <user>" account folder) |
| `company` | copyright line in About |
| `tagline` | app description (taskbar tooltip / OS metadata) |
| `website`, `supportEmail` | About links and the business-licensing link |
| `feedUrl`, `apiBase` | the in-app update check + "Update now" target — point these at the partner's OWN release feed |
| `accentHex` | brand accent — **drives the app icon** (tile + cloud, auto-derived), plus the tray badge and UI accent fallback |
| `appId` | the MSIX Application Id used in the AUMID and the self-update task name — must match the manifest's `<Application Id="…">` |

## Steps to build a branded copy

1. **Edit `internal/brand/brand.json`** — set every field for the partner. Point
   `feedUrl`/`apiBase` at their release hosting (the updater installs from there).

2. **Regenerate the icons.** The app icon and MSIX logos derive automatically
   from `brand.json`'s `accentHex` — no artwork editing. From `shell/windows/appicon`:
   - `go run .` → `cmd/nimbo-gui/assets/nimbo.ico` (window/exe icon), then
     regenerate the resource object so the exe embeds it:
     `windres app.rc -O coff -o rsrc_windows_amd64.syso` in `cmd/nimbo-gui`.
   - `go run . logos ../../../packaging/msix/Assets` → the Store/tile PNGs.

   Preview any accent without touching `brand.json`:
   `go run . -accent "#RRGGBB"` (or `... -accent "#RRGGBB" logos <dir>`).

   Still manual if the partner wants them branded: the Explorer **overlay** icons
   (`shell/windows/overlays/genicons`) aren't accent-driven yet, and a custom logo
   *shape* (the generator recolours the nimbus; a different silhouette means editing
   the generator or dropping in pre-made assets). See the `nimbo-brand` notes.

3. **Edit the MSIX manifest** `packaging/msix/AppxManifest.xml`:
   - `<Identity Name="…" Publisher="…">` — the partner's package name and the
     **signing cert subject** (must match whoever signs it).
   - `<Application Id="…">` — must equal `brand.json` `appId`.
   - `<uap:VisualElements DisplayName="…">` and `<DisplayName>` / `<PublisherDisplayName>`.

4. **Rebuild and sign** with the partner's certificate:
   `release.ps1 -SignSubject "<their cert DN>"` (or `package.ps1`). The exe name
   stays `nimbo-gui.exe`; the updater derives the package name at runtime, so the
   self-update flow follows the new identity automatically.

## Notes

- Changing `Identity Name`/`Publisher` changes the **PackageFamilyName** — a
  branded build is a distinct app from stock Nimbo (separate installs, separate
  updates). That's intended.
- The on-disk config/data dir is branded automatically from `brand.json`'s
  `appId`: stock "Nimbo" → `%AppData%\nimbo` / `%LocalAppData%\nimbo`; a partner
  build → its own slugged folder (e.g. `appId` "AcmeCloud" → `…\acmecloud`), so a
  partner install never shares state with stock Nimbo. No code change needed. The
  CLI executable name (`nimbo.exe`) itself is still unbranded (developer-facing).
- Keep `brand.json` for the stock build committed as Nimbo; produce partner
  builds on a branch or from an out-of-tree overlay so stock releases are never
  accidentally shipped with partner branding.
