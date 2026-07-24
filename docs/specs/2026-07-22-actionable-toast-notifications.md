# Spec: actionable toast notifications via a COM toast activator

**Status:** approved design (revised after adversarial review), ready to plan · **Date:** 2026-07-22

## Background

Nimbo's desktop toasts are currently dead ends. `notify.Toast(title, message, link)`
raises them through `go-toast` with `ActivationType: "protocol"`, where the
activation argument is the **Nextcloud web URL** — so clicking a toast opens a
browser, never Nimbo. A "Sign in required" toast cannot take you to the sign-in
screen, and a Nextcloud notification carrying server-side Accept/Decline actions
offers no way to act on them.

Three further defects surfaced while designing this — all in the toast-*raising*
layer, and all consequences of how `go-toast` builds the notification:

- **`AppID` is not a registered AUMID.** Toasts go out with `AppID: "Nimbo"`.
  They still appear, but are not attributed to the packaged app, which affects
  Action Center persistence and how Nimbo shows in Windows' notification
  settings. **COM activation cannot resolve at all without manifest-backed
  identity** (`packageFamilyName()+"!"+AppID`).
- **`go-toast` is a PowerShell-injection sink, and it reaches server data
  today.** `go-toast` builds the toast by interpolating every field —
  `Title`, `Message`, action `content`, action `arguments` — into a
  **double-quoted PowerShell here-string** (`$template = @"..."@`) that is then
  parsed as XML. Double-quoted here-strings expand `$var`, `$(subexpr)` and
  backtick escapes **at PowerShell parse time, before any XML parsing**. CDATA
  protects only the XML parser, not the PowerShell string layer, so a
  server-supplied notification `Subject`/`Message` containing `$(...)` **executes
  arbitrary PowerShell when the toast is raised — no click required.** This path
  is live in shipping code: `showToast` passes `n.Subject`/`n.Message` straight
  through (`toast_windows.go:36-42`), and that text can be authored by any other
  Nextcloud user (a share note, a filename, a Talk message). This is a genuine
  remote-code-execution trust-boundary violation, not a theoretical one.
- **Structured activation args break go-toast's XML.** The activation payload
  this feature needs (`action=notify&id=5&i=0`) contains a bare `&`, which is
  illegal in an XML attribute. `go-toast` interpolates `launch`/`arguments`
  unescaped, so `XmlDocument.LoadXml` throws. Worse, PowerShell `-File` exits 0
  on that statement-terminating exception, so `Push()` returns `nil` and even the
  `beeep` fallback never fires — **the toast is dropped with no log and no
  output.**

The upshot: the activation design (below) is sound, but the raising layer must be
**replaced**, not patched field-by-field. See "Raising toasts".

## Goal

Toasts route into the right place in Nimbo, and Nextcloud notifications can be
actioned directly from the toast — without introducing (and while closing) the
injection and silent-drop defects above.

## Approach: COM toast activator (not a URI scheme)

The alternative considered was registering a `nimbo:` URI protocol and using
protocol activation for the toast body and buttons. It was rejected because a
registered URI scheme is reachable by **web content**: a page the user is merely
browsing can navigate to `nimbo://notify/5/action/0` and accept a share without
consent. Defending that needs a per-session token plus live-ID validation —
hardening that exists only to patch a hole the approach itself opens. Protocol
activation also routes through the shell and relaunches rather than reaching the
running app.

The COM activator closes the **web** vector: a web page cannot `CoCreateInstance`
a local COM class, and activation reaches the running process directly with no
relaunch.

**Threat model, stated correctly.** It is *not* true that "only Windows can
invoke the activator." A `windows.comServer` registration is, by design, callable
by other processes: any **same-user, medium-integrity** process can
`CoCreateInstance` the CLSID, obtain `INotificationActivationCallback`, and call
`Activate` with crafted args (or cold-launch `nimbo-gui.exe` via the COM launch
path). The no-token decision still stands, but for the correct reason: such an
attacker already runs code as the user, can read the account's app password from
Credential Manager (`internal/account/keychain.go`), and can drive the UI
directly — a token adds nothing against them. The design consequences:

- `Activate` treats `invokedArgs` as **untrusted input**: strict parse,
  unknown/malformed verbs dropped, and every handler performs only actions the
  user could already take from the UI. Argument *values* (`path`, `id`, `i`,
  `acct`) are never assumed pre-validated.
- Optional hardening noted for later: `com:ExeServer` accepts a
  `LaunchAndActivationPermission` SDDL to ACL who may activate. Not required for
  v1; recorded so it isn't rediscovered.

Cost, stated honestly: this is the largest piece of Windows interop in the
codebase, and its dominant failure mode is **silent** — a wrong CLSID, a failed
registration, or a GC'd callback object means buttons simply do nothing. The
design mitigates that with a WARN log line, a capability gate, a manifest/Go
CLSID cross-check test, a debug flag, and a day-one activation spike (see
Testing).

## Architecture

### Package boundaries (activator lives in the GUI, not `internal/notify`)

`internal/notify` is a **Wails-free leaf package**. It is pulled into the pure-Go
CLI via `cmd/nimbo → internal/cli → internal/agent → internal/notify` and into
the gomobile facade via `mobile/mobile.go`. Therefore:

- The activator's COM plumbing that must call `application.InvokeAsync` and the
  verb→handler dispatch table live in **`cmd/nimbo-gui`** (package `main`),
  next to the `*App` handler methods (`showLogin`, `OpenStatusTab`, `ApplyUpdate`),
  which `internal/notify` cannot import anyway.
- `internal/notify` exposes **pure, OS-independent, Wails-free** helpers only:
  the argument encode/parse functions, the sanitiser, and the toast-XML builder.
- The COM registration and `INotificationActivationCallback` implementation go in
  a new **`cmd/nimbo-gui/toastactivator_windows.go`** (Windows-tagged), with a
  `toastactivator_other.go` no-op. This keeps Wails imports out of the CLI and
  mobile builds entirely.

### The COM activator (`cmd/nimbo-gui/toastactivator_windows.go`)

A COM local server implementing `INotificationActivationCallback` (IID
`53E31837-6600-4A81-9395-75CFFE746F94`, inherits `IUnknown`):

```
HRESULT Activate(
  [in] LPCWSTR appUserModelId,
  [in] LPCWSTR invokedArgs,
  [in] const NOTIFICATION_USER_INPUT_DATA *data,
  [in] ULONG count)
```

- The **ToastActivatorCLSID is `00EEDCE7-5C4E-4573-85C2-98790F8F98AE`**, generated
  for this spec and **permanent** *per app identity*. It lives in the manifest and
  forms part of the app's toast identity — changing it later orphans outstanding
  notifications, the way changing the Publisher orphans installs. Treat it like
  the Store identity values: do not change it. (White-label builds get their own
  per-partner GUID — see "White-label".)
- **Single source of truth:** the GUID is a Go constant
  (`toastActivatorCLSID`); the manifest values are asserted equal to it by a unit
  test (see Testing). A manifest/Go mismatch is the archetypal silent failure —
  registration succeeds against the Go constant while Windows activates the
  manifest's different CLSID — so it must be caught mechanically.
- Two hand-built vtables (`IClassFactory`, `INotificationActivationCallback`) via
  `syscall.NewCallback`, registered with
  `CoRegisterClassObject(CLSID, factory, CLSCTX_LOCAL_SERVER, REGCLS_MULTIPLEUSE)`.
- **GC lifetime (mandatory):** the factory object, the activator object, their
  vtable structs, and all `syscall.NewCallback` trampolines are **package-level
  variables**, created once at init, alive for the whole process. COM holds only
  raw pointers, invisible to the Go GC; locals would be collected and a later
  click would dereference freed memory. Follow the existing house idiom in
  `internal/cfapi/cfapi_windows.go` (package-level `NewCallback` vars +
  `runtime.KeepAlive`). `NewCallback` trampolines are drawn from a fixed
  process-wide pool and never released, which is fine at ~a-handful created once,
  but forbids per-toast/per-registration creation.
- Registration runs on a **dedicated OS-locked goroutine with
  `CoInitializeEx(MTA)`** — deliberately *not* the Wails UI thread, which is an
  STA pumping the window message loop; taking COM callbacks there invites
  reentrancy deadlocks.
- `Activate` parses `invokedArgs` (via the `internal/notify` parser) and hands the
  parsed action to the UI thread via `application.InvokeAsync`, the same
  marshalling `onSecondInstance` already uses. `data`/`count` are unused — no
  inline text inputs.

#### COM contract the hand-built server must honour

Each of these fails **silently** (Windows drops the activation) if wrong, so they
are specified rather than left implicit:

- `QueryInterface` succeeds for `IID_IUnknown` plus the object's own interface
  (`IID_IClassFactory` / `IID_INotificationActivationCallback`), `AddRef`s through
  the returned pointer, and returns `E_NOINTERFACE` with `*ppv = NULL` otherwise
  (Windows *will* QI for `IMarshal`/`IAgileObject` — these must cleanly fail).
- `AddRef`/`Release` return plausible counts; the objects are static-lifetime, so
  refcounting is effectively a no-op but must not misbehave.
- `IClassFactory::CreateInstance` returns `CLASS_E_NOAGGREGATION` when
  `pUnkOuter != NULL`, and QIs the activator for the requested `riid`.
- `Activate` returns `S_OK`.
- GUIDs are compared **by binary struct value** (`Data1` little-endian), never by
  string.

**cgo:** the GUI builds `CGO_ENABLED=1` (Wails/cfapi need it), which is fine and
should be preserved. Note for accuracy: cgo is **not** actually required for
`syscall.NewCallback` on foreign (COM RPC) threads — the Go runtime special-cases
Windows (`iscgo || GOOS == "windows"`), which is how `x/sys/windows/svc` works in
pure-Go builds. Do not encode a false cgo dependency.

### Manifest (`packaging/msix/AppxManifest.xml`)

Add the toast-activation extension and an `ExeServer` **inside the existing
`com:ComServer`** (Microsoft's schema explicitly discourages a second
`com:Extension` per Application; `ExeServer` and `SurrogateServer` may be siblings
under one `com:ComServer`, which also sidesteps the "first entry" gotcha):

```xml
<desktop:Extension Category="windows.toastNotificationActivation">
  <desktop:ToastNotificationActivation ToastActivatorCLSID="00EEDCE7-5C4E-4573-85C2-98790F8F98AE" />
</desktop:Extension>

<!-- inside the EXISTING <com:Extension Category="windows.comServer"><com:ComServer> -->
  <com:ExeServer Executable="nimbo-gui.exe" Arguments="-ToastActivated" DisplayName="Nimbo toast activator">
    <com:Class Id="00EEDCE7-5C4E-4573-85C2-98790F8F98AE" />
  </com:ExeServer>
  <!-- the existing <com:SurrogateServer> for NimboCtxMenu.dll stays as a sibling -->
```

The `desktop` namespace is already declared. Manual regression test: confirm the
Explorer context menu still works after the change (shared `com:ComServer`).

### Raising toasts (native in-process WinRT — no PowerShell, no go-toast)

**`go-toast` is removed from the Windows raise path.** Toasts are raised natively
in-process via WinRT — the way Windows itself does it — with no child process and
no script. This lives in a new Windows-tagged file
**`internal/notify/toast_raise_windows.go`**; the package stays Wails-free, using
only `golang.org/x/sys/windows` + `combase.dll` syscalls, following the house
idiom already shipping in `internal/cfapi/cfapi_windows.go` and
`cmd/nimbo-gui/shortcut_windows.go` (`NewLazySystemDLL`/`NewProc`, hand-declared
GUIDs, `syscall.SyscallN` on vtable slots, `runtime.KeepAlive`). No cgo, no C++,
no Windows SDK headers, no new module, no Wails bindings.

1. **Build the toast XML in Go** with correct escaping — attribute values
   attribute-escaped, element text text-escaped (`encoding/xml` or a single
   audited escape helper). No CDATA, no reliance on the caller pre-escaping. The
   escaping now guards **`XmlDocument.LoadXml` well-formedness** (a bare `&` in
   `action=notify&id=5&i=0` still throws in `LoadXml`), not PowerShell inertness —
   there is no script layer to be inert.
2. **Raise it via the WinRT object graph.** On a dedicated `runtime.LockOSThread`
   goroutine that has called `RoInitialize(RO_INIT_MULTITHREADED)` (never the
   Wails STA UI thread — reentrancy; may reuse the activator's MTA thread):
   - `RoActivateInstance("Windows.Data.Xml.Dom.XmlDocument")` → QI
     `IID_IXmlDocumentIO` → `LoadXml(HSTRING xml)`;
   - `RoGetActivationFactory("Windows.UI.Notifications.ToastNotification",
     IID_IToastNotificationFactory)` → `CreateToastNotification(xmlDoc)` →
     `IToastNotification` (optionally QI `IToastNotification2` for
     `put_Tag`/`put_Group`);
   - `RoGetActivationFactory("Windows.UI.Notifications.ToastNotificationManager",
     IID_IToastNotificationManagerStatics)` → `CreateToastNotifierWithId(HSTRING
     AUMID)` → `IToastNotifier::Show(toast)`.

   HSTRINGs are created with `WindowsCreateString` and freed with
   `WindowsDeleteString` via an **own helper sized by
   `len(utf16.Encode([]rune(s)))`** — the UTF-16 **code-unit** count, not the rune
   count — so emoji / non-BMP text in a server `Subject`/`Message` isn't
   corrupted (this is exactly the bug in `go-ole`'s `NewHString`, which is why we
   hand-roll rather than borrow it). Every interface is `Release()`d; every
   argument is `runtime.KeepAlive`'d across each `syscall.SyscallN` on its verified
   vtable slot. The only interpolated app-controlled values (AUMID, Tag) are
   passed as HSTRING **method arguments**, never concatenated into any string that
   is later parsed — so the entire injection class is gone by construction, not by
   escaping.

   Check every HRESULT and WARN-log failures. Note `IToastNotifier::Show` can
   return `S_OK` yet suppress the toast (AUMID miss, notifications off), so `S_OK`
   is not proof of display; optionally consult `IToastNotifier::get_Setting`.

**Implementation note (decided):** hand-roll the ~6 `combase.dll` procs
(`RoInitialize`, `RoUninitialize`, `RoGetActivationFactory`, `RoActivateInstance`,
`WindowsCreateString`, `WindowsDeleteString`) rather than promoting `go-ole` to a
direct dependency. Rationale: no `go.mod` change, the toast code owns its
dependency surface (Wails currently pulls `go-ole` transitively; a future bump
could drop it), and it avoids `go-ole`'s rune-count HSTRING bug. The runner-up —
adopt `git.sr.ht/~jackmordaunt/go-toast`'s `wintoast` (already indirect via
`beeep`) — was rejected because its WinRT bindings sit under `internal/` (Go
forbids importing them), its only public entry `wintoast.Push` registers *its own*
activator class factory as a side effect (competing with our manifest CLSID and
pushing COM registration into the leaf package), and it's a single-maintainer
untagged project.

**AUMID:**
- packaged → `packageFamilyName()+"!"+brand.Current.AppID` (the real AUMID),
  used **whether or not COM registration succeeded** — attribution is independent
  of activation.
- unpackaged/dev → keep `"Nimbo"` (the current literal); the whole activation
  feature is gated off there anyway.

`packageFamilyName()` moves from `cmd/nimbo-gui/restart_windows.go` to a shared
location reachable by `internal/notify/toast_raise_windows.go` (which builds
`pfn+"!"+brand.Current.AppID`).

**Tag/Group for retirement:** every notification toast carries `Tag =
<notification id>` (and a fixed `Group`), set via `IToastNotification2`'s
`put_Tag`/`put_Group`, so the Notifier can retire it natively via
`IToastNotificationManagerStatics2::get_History` →
`IToastNotificationHistory::RemoveGroupedTagWithId(tag, group, AUMID)` when an id
leaves the fetched set — no shell-out. This retires toasts for notifications
actioned in-app or on another device; a viable follow-up (~1 extra interface).
Nimbo's own toasts carry stable tags too where retirement makes sense.

**Raising API.** `internal/notify` gains a rich entry point used by call sites:

```go
notify.Raise(ToastSpec{Kind, Title, Message, BodyArgs, Buttons []ToastButton, Tag})
```

- `Title`/`Message` are escaped inside the builder; callers pass raw text.
- `notify.Toast(title, message, link)` is **kept unchanged** as a thin
  compatibility wrapper (legacy call sites, and the engine callback below), so the
  gomobile contract is untouched.
- The Nextcloud-notification toast is raised by the unexported `showToast(n
  transport.Notification)` **inside** `internal/notify`, which already has the
  full struct (id, actions, link) — buttons never cross a package boundary.
- Nimbo's own buttoned toasts (Sign in, Update, On-demand error) are raised
  **GUI-side** in `cmd/nimbo-gui`, free to build any `ToastSpec`.

### Sync-conflict "Show file" data flow

The conflict toast has two raise sites; the abs local path is available at both
but must not be forced through the frozen 3-string `SetToastFunc`/mobile
`OnToast` callback:

- **On-demand mode (GUI, `service.go:~905`):** the enclosing closure already
  receives the absolute `localPath`; build the `showfile&path=<abs>` `ToastSpec`
  directly.
- **Live-sync mode (engine):** add an **additive** engine hook
  (`SetConflictToastFunc(func(localAbs, remote string))`) that the GUI wires to a
  rich-toast raise. This leaves `SetToastFunc`'s signature (and the mobile
  contract) frozen; mobile simply doesn't set the new hook.

`showfile` is invoked with the existing argv-based `revealPath` helper
(`exec.Command("explorer", "/select,"+path)`) — **never** a `cmd /c`
concatenation. If the file is gone at click time, open its containing folder,
else fall back to `status`.

### Activation flow

- **App running, factory registered** — Windows calls `Activate` in-process. No
  relaunch, no second instance.
- **App not running** — Windows launches `nimbo-gui.exe -ToastActivated`; the app
  registers the factory **early** (see below) and Windows then calls `Activate`.
  The action is queued until the engine is up, reusing the existing
  `pendingShare`/`pendingApp` pattern.
- **App starting, or running with registration failed (the third state the first
  draft missed)** — Wails' `SingleInstance` calls `os.Exit` inside
  `application.New` when the lock is held, *before* any later init. If COM's
  `-ToastActivated` launch hits that window (Nimbo is a logon `startupTask`,
  exactly when users click backlogged Action Center toasts), the second process
  dies before registering and the click is silently lost. Mitigations, all
  required:
  - **Register the class factory at the very top of `main()`**, before
    `application.New`/engine/frontend init, so the unregistered window is
    milliseconds and COM's server-launch deadline is met.
  - Define `-ToastActivated` **second-instance** behaviour: if a running instance
    receives the forwarded flag, it logs at WARN and **shows/focuses the flyout**
    so the click is never a dead end (the action payload itself only ever arrives
    via the in-process `Activate`, so it cannot be forwarded).
  - The debug flag is named `--debug-toast-args` (distinct from the real
    `-ToastActivated`, and matched via `argValue`, never `flag.Parse`) to avoid
    conflation.
  - After a *failed* registration, foreground toasts persisted from earlier
    sessions will dead-click; accepted as a documented edge (they expire in ≤3
    days), with the WARN log as the breadcrumb.

### No new Wails bindings

Every handler calls existing internal Go methods (`showLogin`, `OpenStatusTab`,
`ApplyUpdate`, `revealPath`). This keeps the change clear of the bindings-regen
hazard that broke release 0.1.0.97. The notification body-click carries its id via
the existing `OpenStatusTab(tab string)` argument (encoded string, e.g.
`"notifications\nnotif\n<id>"`), which the Status window already understands — no
new binding.

## Activation argument format

An opaque string we own, in URL query syntax, parsed with `net/url.ParseQuery`.
XML-attribute-escaped at embed time (see Raising); Windows returns the unescaped
form to `Activate`.

```
action=login&acct=<account id>
action=notifications&id=<notification id>          # id optional: highlights the row
action=notifications&acct=<account id>&id=<id>
action=update
action=settings
action=status
action=showfile&path=<abs path>
action=notify&acct=<account id>&id=<notification id>&label=<action label>
```

- **Account discriminator (`acct`).** Each secondary account runs its own engine,
  Notifier and toasts; notification ids are per-server and collide across
  accounts. `acct` lets the handler pick the right `transport.Client`. The raiser
  always knows its account. Absent `acct` → fall back to the primary.
- **Action binding by label, not index.** `notify` carries `label=<action
  label>` (URL-escaped), resolved via the existing `DoNotificationAction(id,
  label)` (matches id then label). This is strictly more robust than a positional
  index and reuses shipped code; on no-match the handler degrades to opening the
  notifications tab. (Nextcloud notifications are immutable per-id so an index
  would *usually* be safe, but label binding removes the failure mode for free.)
- Argument **values are untrusted** (see threat model): parsed strictly, and the
  `showfile` path is invoked via argv and, defence-in-depth, checked for
  containment under a known sync-pair/mount root before revealing.

## Handler table

Body-click routing is defined for **every** toast, not just the five below: any
toast not listed (there are ~15 other `notify.Toast` call sites across
`appwindows.go`/`service.go`) gets body-click `action=status` (focus the app),
and the dispatcher treats **empty or unknown args as that same `status`
default** — never a silent no-op, which would be indistinguishable from a broken
activator.

| Toast | Body click | Buttons |
| --- | --- | --- |
| Sign in required | `login&acct=…` | **Sign in** → `login&acct=…` |
| Update available | `settings` | **Update & restart** → `update`; **Later** → dismiss |
| Sync conflict | `showfile&path=…` | **Show file** → same |
| On-demand error | `status` | **Open Nimbo** → `status` |
| Nextcloud notification | `notifications&acct=…&id=…` | server actions (Primary first) + **Open in browser** |
| *(all other toasts)* | `status` | — |

Nextcloud notification button rules (resolves the earlier vacuous "if within the
cap"):

- Order server actions **Primary first**, then array order.
- **0 actions:** `[Open in browser]` if `Link` present, else no buttons.
- **1 action:** `[action, Open in browser?]` (browser added if `Link` present).
- **2 actions:** `[action, action]`; add **Open in browser** only if `Link`
  present *and* it still fits the 3-button cap (2 actions + browser = 3 = fits).
- **3+ actions:** first two by (Primary, array order), then **Open in browser**
  if `Link` present, else the third slot is a **More…** button → `notifications`
  so dropped actions remain reachable in-app.
- **Cap at 3 buttons** (Windows allows 5 but they truncate badly).

Other notes:

- **Later** uses `activationType="system"` `arguments="dismiss"`.
- **Open in browser** stays `protocol` activation, but its `Link` is validated
  first (see below); mixed activation types per button are legal.
- The **sign-in toast** for a *secondary* account carries that account's `acct`,
  so "Sign in" re-auths the failed account rather than disturbing the healthy
  primary.

## Error handling

- **Server action fails → follow-up toast naming the reason.** This feedback
  **bypasses the `enabled` gate and any rate limiter**: it answers a direct user
  gesture, so it must not be swallowed when ambient notifications are disabled or
  within an 8-second on-demand window. "Naming the reason" means a summarised
  message — `ExecuteAction`'s error embeds the full request URL, which must not be
  shown raw.
- **Notification already gone (actioned elsewhere) →** refresh the list, retire
  the toast via its Tag, **no** error toast.
- **Cold-start action needing the engine →** queued; if the account needs auth,
  degrade to opening the login window.
- **"Update & restart" from a toast:** route `ApplyUpdate`'s non-empty return
  string into a **follow-up toast** ("Couldn't update: …", or "You're already on
  the latest version" — which also retires the stale toast). Before quitting on
  success, raise an **"Updating — Nimbo will restart"** toast so the app's
  disappearance is explained (the Settings "Updating…" state is invisible from a
  toast). Guard re-entrancy with an **atomic `updateInFlight` flag** on `App`,
  set at the top of `ApplyUpdate` and cleared on error return — serving both the
  toast button and the Settings button (the spec's earlier "idempotent" claim
  described a guard that does not exist).
- **Server `Link` used as protocol activation** (Open-in-browser, and the
  registration-failed fallback): the `Link` is server-supplied, and protocol
  activation `ShellExecute`s *any* scheme (`file://` UNC → NTLM emission, `ms-*`,
  third-party handlers). Require the `Link` to parse as **http/https** (ideally
  same scheme+host as the account's server) before using it; otherwise drop the
  button / degrade to a plain toast.
- **COM registration fails** (unpackaged, or `CoRegisterClassObject` error) → log
  at **WARN** and fall back to plain (buttonless) toasts. The **AUMID attribution
  fix still applies** when packaged; only activation is disabled. Gate activation
  on `packaged && registrationSucceeded`.
- **Raise-time failure** (a WinRT `HRESULT` error from
  `LoadXml`/`CreateToastNotifierWithId`/`Show`, or notifications disabled): log at
  **WARN** naming the dropped actionability — never a `Debug`-only silent degrade.
  Check every HRESULT; treat `Show() == S_OK` as *sent, not necessarily shown*.

## Testing

Logic is deliberately pushed *out* of the COM layer, which resists unit testing.

**Day-one de-risking spikes (before building the real vtables).** Two unknowns
gate the whole design; prove both on a real MSIX install first:

1. **Activation reaches us.** Does a toast raised **in-process via the native
   WinRT raiser** (`CreateToastNotifierWithId(pfn!AppID).Show`) actually invoke
   the manifest `ToastActivatorCLSID` on click? Install a manifest with the CLSID
   + a **stub** exe COM server that just logs `Activate`, raise one toast via the
   native raiser, and confirm the stub is called (app running *and* closed). If
   the explicit-AUMID `CreateToastNotifierWithId` is rejected from an
   identity-bearing caller, fall back to the parameterless `CreateToastNotifier()`
   bound to the current package identity (a one-line change).
2. **The interface contract is right.** Verify the exact IIDs and vtable slot
   indices for `IToastNotificationManagerStatics`, `IToastNotifier`,
   `IToastNotificationFactory`, `IToastNotification`(+`IToastNotification2`) and
   `IXmlDocumentIO` against `Windows.UI.Notifications.h` / the winmd, and get one
   real toast to display end-to-end. A mistranscribed IID or slot is a hard crash
   or (for void returns) a silent no-op — the flagged failure class.

Only after both pass is the full activator + button wiring worth building.

**Unit tests (OS-independent, plain `go test`):**
- argument encode/parse round-trip, including malformed/unknown verbs, missing
  `acct`, and the `&`-bearing `notify` payload **through the actual XML builder
  and back** (guards the escaping regression).
- toast-XML builder escaping: `Title`/`Message`/label/args containing `"`, `<`,
  `>`, `&`, `$(...)`, backtick, `]]>`, and a line-leading `"@` — asserting the
  built XML is well-formed and correctly escaped so `XmlDocument.LoadXml` accepts
  it. There is no script layer anymore; escaping guards `LoadXml`
  well-formedness. The `$(...)`/backtick/`"@` cases are now ordinary text,
  documenting that the injection surface is gone by construction.
- verb→handler dispatch table, exercised with fakes.
- **manifest/Go CLSID cross-check:** a test that parses
  `packaging/msix/AppxManifest.xml` and asserts both the `ToastActivatorCLSID`
  and the `com:Class Id` equal the Go `toastActivatorCLSID` constant.

**Debug hook:** the hidden `--debug-toast-args=<args>` flag feeds the identical
dispatch path without COM, so every handler is exercisable in a `go run` session.

**Manual, on a real MSIX install** (matching the project's release-based testing):
- activation with the app running, **during app startup**, and with the app
  closed;
- each button on each toast type;
- Action Center persistence after the toast expires, and toast **retirement**
  when a notification is actioned in-app;
- multi-account: a secondary-account notification's Accept/Decline acts on the
  right account; the secondary "Sign in" toast re-auths the right account;
- notification settings shows exactly one usable Nimbo entry;
- the existing Explorer context menu still works (shared-manifest regression).

## White-label

`packaging/whitelabel/build-partner.ps1` patches identity strings per partner but
does **not** touch `com:` elements today. Two additions required so partner builds
don't ship Nimbo's identity:

- Patch the toast-activator `DisplayName` (currently `"Nimbo toast activator"`).
- Give each partner **its own `ToastActivatorCLSID`** (a per-partner GUID in
  `partner.json`, patched into both manifest entries and surfaced to the Go
  constant at build time). A shared CLSID across distinct package identities lets
  two co-installed clients declare the same COM class and misroute activation.
  The "permanent" rule is *per app identity*, not one global GUID.

## Migration note

Switching `AppID` from `"Nimbo"` to the real AUMID creates a new notification
identity in Windows. Old Action Center items under the pseudo-AppID expire within
~3 days (Windows caps toast lifetime), so nothing needs active clearing, but
per-app Windows notification preferences do **not** carry over — worth a
release-notes line. This is the "do identity changes before there's a user base"
window the project already applies to the Publisher and the CLSID; the install
base is pre-launch.

## Out of scope

- **Building the toast XML via the `XmlDocument` DOM API**
  (`CreateElement`/`SetAttribute`/`AppendChild`) instead of `LoadXml`. `LoadXml`
  with one audited escape helper is the chosen sweet spot; DOM construction costs
  many more vtable calls per toast for no security gain (with no shell present,
  neither can execute code).
- **Raising via a C++/WinRT helper** — explicitly rejected: the repo's w64devkit
  MinGW toolchain cannot build cppwinrt (no `winrt/*.h`), and MSVC-compiled
  objects cannot link into a MinGW cgo binary. A companion DLL built with MSVC
  would add `cl.exe` + the Windows SDK C++ workload as a new build prerequisite
  the repo deliberately avoids (every existing C++ artifact is MinGW g++,
  static-linked). Any PowerShell-based raise is likewise rejected.
- Inline toast inputs (text fields, dropdowns). `data`/`count` are accepted and
  ignored.
- `LaunchAndActivationPermission` SDDL on the ExeServer (noted as available
  hardening; not v1).
- Non-Windows platforms. `toast_other.go` keeps its no-op behaviour (and remains
  the only place `beeep`/`go-toast` are used).
