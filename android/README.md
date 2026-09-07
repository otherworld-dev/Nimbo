# Nimbo for Android

A native Android client for Nextcloud built on [Nimbo](..)'s sync
engine, focused on what the official Android app does poorly: **reliable
two-way folder sync**, **folder monitoring**, and **instant push-driven
updates** — plus launching your server's Nextcloud web apps (Deck, Notes,
Calendar, …) so they feel like installed native apps.

## How this app relates to the engine

The sync engine (three-way diff, rename detection, chunked/resumable
transfers, conflict handling, `notify_push`) lives at the root of this repo
and is *not* copied here. Go forbids importing another module's `internal/`
packages, and copying would fork the engine — sync fixes would have to land
twice. Instead:

- The repo root carries a small [`mobile/`](../mobile/mobile.go) package —
  the gomobile facade. It may use the internals because it lives in the same
  module. Engine improvements automatically improve Android.
- **This directory** holds the Android app (Kotlin) and consumes the facade as
  a prebuilt Android library: `core/nimbo-core.aar`, produced by
  `scripts/build-core.ps1` from the Go source in this same repo.

```
repo root                             android/ (this directory)
  internal/…   sync engine              app/        Kotlin app (Compose UI,
  mobile/      gomobile facade  ──►     core/       nimbo-core.aar (built,
               (gomobile bind)                      gitignored)
```

The facade API crosses the JNI boundary as: methods with primitive/string
parameters, JSON strings for collections, and two Kotlin-implemented
interfaces — `SecretStore` (Android Keystore-backed password storage) and
`Listener` (engine events: status, progress, toasts, conflicts, auth-lost).

## Layout

```
app/        Android application (Kotlin, Jetpack Compose)
  core/       the JNI bridge: NimboCore, Keystore-backed SecretStore, engine
              Listener, JSON models for the facade's payloads
  service/    dataSync foreground service, WorkManager periodic sync, notifications
  platform/   permissions (All-files access, notifications, battery) + local
              filesystem browsing
  ui/         Compose screens, one UiState, a Route enum instead of a nav library
core/       drop point for nimbo-core.aar + sources jar (gitignored)
docs/       DESIGN.md — platform design: storage, background sync, web-app launcher
scripts/    build-core.ps1 — builds the .aar from the repo's mobile/ facade
```

## Building the core library

Prerequisites: Go, `gomobile`/`gobind` (`go install golang.org/x/mobile/cmd/...@latest`),
and an Android SDK with an NDK under `%ANDROID_HOME%\ndk\`. The Go engine is
in this same repo, so no other checkout is needed.

```powershell
scripts/build-core.ps1        # -> core/nimbo-core.aar
```

## Building the app

```powershell
scripts/build-core.ps1 -Targets android/arm64   # the Go core (fast dev loop)
./gradlew :app:assembleDebug                    # -> app/build/outputs/apk/debug/
```

`local.properties` (gitignored) must point `sdk.dir` at the Android SDK. The
committed toolchain is AGP 8.7.3 / Gradle 8.11.1 / Kotlin 2.0.21 / JDK 17,
compileSdk 35, minSdk 26.

The debug build is **arm64-v8a only**, matching the `.aar` the dev-loop script
produces. Run `scripts/build-core.ps1` with no `-Targets` for all four ABIs
before shipping anything.

## Status

**Milestone 2 (walking skeleton) is done and running on hardware.** Sign-in via
Login Flow v2, All-files-access onboarding, a remote folder browser, choosing
where each folder lands locally, a `dataSync` foreground service hosting the
engine's run loop, live status/progress, sync-now/pause, quota, the Nextcloud
app launcher and a diagnostics screen. Verified end-to-end on a Galaxy S24 Ultra
(Android 16) against a live Nextcloud: signed in, added a folder, synced.

Still open, roughly in milestone order:

- **Milestone 3** — the Doze/OEM testing matrix, and event triggers
  (connectivity regained, charging) beyond the 15-minute WorkManager baseline.
- **Milestone 4** — selective sync, camera-roll preset, MediaStore observer.
- **Milestone 5** — the app launcher currently opens each Nextcloud app in a
  Custom Tab (which inherits the sign-in session). WebView auto-login, per-app
  home-screen shortcuts and server theming are still to do.
- **Milestone 6** — conflict UI (v1 runs `PolicyAuto`, keep-both), multi-account,
  billing, Play declarations. R8 is off; enabling it needs JNI keep rules.

Known engine issue: the core persists settings by writing a fixed
`<file>.tmp` and renaming it, so two concurrent saves can lose the rename with
`ENOENT` — observed on device as `SetBaseDir` failing right after `Start`. The
app retries and verifies as a workaround (`NimboCore.setBaseDirWithRetry`); the
real fix is a unique temp name in `internal/config`, which also affects desktop.

## License

[PolyForm Noncommercial 1.0.0](LICENSE), matching Nimbo — source available for
any noncommercial use; commercial use and resale need a licence from
Otherworld Dev Ltd. The app itself is distributed commercially by Otherworld
Dev (which, as licensor, is not bound by the noncommercial restriction).

Nimbo is an independent client, not affiliated with or endorsed by Nextcloud GmbH.
