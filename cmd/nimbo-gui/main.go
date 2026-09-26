// Command nimbo-gui is the Nimbo desktop app (Wails v3): a tray icon
// with an attached OneDrive-style flyout panel, backed by the shared sync engine.
package main

import (
	"context"
	"embed"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"github.com/otherworld/nimbo/internal/account"
	"github.com/otherworld/nimbo/internal/applog"
	"github.com/otherworld/nimbo/internal/brand"
	"github.com/otherworld/nimbo/internal/cfapi"
	"github.com/otherworld/nimbo/internal/config"
	"github.com/otherworld/nimbo/internal/shellmenu"
)

//go:embed all:frontend/dist
var assets embed.FS

//go:embed assets/nimbo.ico
var navIconICO []byte

// version is the build version, set via -ldflags "-X main.version=v1.2.3".
var version = "dev"

// channel is the distribution channel, set via -ldflags "-X main.channel=store"
// for the Microsoft Store build. The Store updates the app itself, so a Store
// build must NOT run the in-app MSIX self-updater (it would fail and breaches
// Store policy). Default "direct" = the self-updating direct-download build.
var channel = "direct"

// isStoreBuild reports whether this is the Microsoft Store distribution build.
func isStoreBuild() bool { return channel == "store" }

// wailsLogger is the logger handed to Wails: Nimbo's own, with Wails' lines
// marked src=wails. Call it after the log is set up.
func wailsLogger() *slog.Logger { return slog.Default().With("src", "wails") }

// hasAccount reports whether an account is configured AND its app password is
// still in the keychain. An account whose secret has vanished (wiped store,
// profile trouble) must take the sign-in path here: starting the engine can
// only fail, and the failure handler's InvokeAsync panics this early — the
// Wails main loop is not running yet (crashed the VM on 2026-08-21).
func hasAccount() bool {
	d, err := config.Resolve()
	if err != nil {
		return false
	}
	st, err := account.LoadStore(d.AccountsFile())
	if err != nil {
		return false
	}
	acc, ok := st.Default()
	if !ok {
		return false
	}
	if _, err := account.LoadSecret(acc.ID); err != nil {
		slog.Warn("account has no stored app password; asking for a sign-in", "err", err)
		return false
	}
	return true
}

// flyoutHeight is the fixed height of the tray flyout panel; its width follows
// the panel-width appearance setting (see flyoutWidthFor).
const flyoutHeight = 500

func main() {
	// FIRST, before anything can create a window: declare per-monitor DPI
	// awareness. Windows latches the process default at first use, so this has
	// to precede all Wails setup or the UI renders system-scaled and blurry
	// above 100% display scaling (Deck #552).
	if err := setDPIAwareness(); err != nil {
		// Not fatal — we just render as before, so log it and carry on. (slog
		// isn't configured yet; this goes to the default handler on stderr.)
		slog.Debug("could not set per-monitor DPI awareness", "err", err)
	}
	// See cloud-file state truthfully: without this, cfapi DISGUISES reparse
	// points from us wherever we are not the connected provider (live-mode
	// status roots always, on-demand briefly), and every attribute probe lies —
	// the live status walk re-converted already-converted files forever.
	cfapi.ExposePlaceholders()

	// Logging: stderr + a rotating file under the data dir, so the windowless
	// build still leaves a diagnosable trail. Verbosity comes from settings or
	// the NEXTCLIENT_DEBUG env var.
	verbose := os.Getenv("NEXTCLIENT_DEBUG") != ""
	if d, err := config.Resolve(); err == nil {
		if s, _ := d.LoadSettings(); s.VerboseLog {
			verbose = true
		}
		if lerr := applog.Setup(d.LogFile(), verbose); lerr != nil {
			slog.Warn("file logging unavailable, using stderr only", "err", lerr)
		}
		setupCrashLog(filepath.Join(filepath.Dir(d.LogFile()), "crash.log"))
	} else {
		applog.SetVerbose(verbose)
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: applog.Level()})))
	}

	ctx := context.Background()
	svc := &App{ctx: ctx}
	app := application.New(application.Options{
		Name:        brand.Current.Name,
		Description: brand.Current.Tagline,
		Services:    []application.Service{application.NewService(svc)},
		Assets:      application.AssetOptions{Handler: application.AssetFileServerFS(assets)},
		// App windows host pages from the user's own server, and those pages
		// talk back through raw postMessage — the one channel that doesn't
		// need a bound method (and so can't disturb the generated bindings).
		// Today that's only "open this link in the real browser".
		RawMessageHandler: svc.handleRawMessage,
		// Wails' own messages (a WebView2 process rebuilt, a bound method that
		// panicked) go to Nimbo's log; left alone they go to a stderr the
		// windowsgui build doesn't have.
		Logger: wailsLogger(),
		// Single instance: a second launch (e.g. the Explorer "Share" menu running
		// `nimbo-gui --share <path>`) forwards its args to the running tray
		// app and exits, rather than starting a duplicate.
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: "dev.otherworld.nimbo",
			OnSecondInstanceLaunch: func(d application.SecondInstanceData) {
				svc.onSecondInstance(d.Args)
			},
		},
	})
	svc.app = app
	// Lets work that must go through the main loop wait for it (afterStart).
	svc.started = make(chan struct{})
	var startedOnce sync.Once
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		startedOnce.Do(func() { close(svc.started) })
	})
	// Logged here, not earlier: a second launch (Explorer's Share menu) hands
	// its arguments to the running app and exits inside application.New, and
	// must not read as a start that never logged "exiting".
	slog.Info("starting", "version", version, "pid", os.Getpid())

	// Toast activation: register the COM callback so clicking a toast (or a toast
	// button) routes to the matching action (sign in, open notifications, run a
	// notification's Accept/Decline, etc.).
	toastActivationHandler = svc.dispatchToastActivation
	registerToastActivator()

	// Register the Explorer "Share with Nimbo" context-menu entry, pointing
	// at the current executable (best-effort; ignored where unsupported).
	if exe, err := os.Executable(); err == nil {
		if rerr := shellmenu.Register(exe); rerr != nil {
			slog.Warn("could not register Explorer share menu", "err", rerr)
		}
	}

	// A share / version-history request at launch (Explorer menu with no instance
	// yet running) is queued and handled once the engine is up.
	if p := argValue(os.Args, "--share"); p != "" {
		svc.pendingShare = p
	}
	if p := argValue(os.Args, "--versions"); p != "" {
		svc.pendingVersions = p
	}
	if p := argValue(os.Args, "--keep"); p != "" {
		svc.pendingKeep = p
	}
	if p := argValue(os.Args, "--free"); p != "" {
		svc.pendingFree = p
	}
	if p := argValue(os.Args, "--app"); p != "" {
		svc.pendingApp = p // a Start-menu app shortcut launched us
	}

	// The flyout: a frameless panel attached to the tray icon (shows by the tray
	// on click, OneDrive-style). Its width follows the panel-width appearance
	// setting; the height is fixed.
	flyout := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:             "flyout",
		Width:            flyoutWidthFor(svc.FlyoutAppearance().PanelWidth),
		Height:           flyoutHeight,
		Frameless:        true,
		AlwaysOnTop:      true,
		DisableResize:    true,
		Hidden:           true,
		URL:              "/",
		BackgroundColour: application.NewRGB(255, 255, 255),
	})
	svc.flyout = flyout

	// Dismiss the flyout when it loses focus (click-away), like OneDrive's panel.
	flyout.OnWindowEvent(events.Common.WindowLostFocus, func(*application.WindowEvent) {
		flyout.Hide()
	})
	tray := app.SystemTray.New()
	tray.SetIcon(trayIcon("idle", 0, false))
	// The icon had no tooltip, so hovering it showed nothing. Every later icon
	// change has to put it back (setTrayIcon). The name in the taskbar settings
	// comes from the exe's version resource instead (versioninfo.rc, GitHub #9).
	tray.SetTooltip(brand.Current.Name)
	tray.AttachWindow(flyout)
	svc.tray = tray

	// Re-assert the logical size AND re-anchor to the tray whenever the panel is
	// shown or the display scale changes.
	//
	// Order matters, and so does the re-anchor. Wails anchors an attached window's
	// BOTTOM-RIGHT to the work area, computing it from the window's size at that
	// instant, and it does so on the tray click BEFORE Show(). Our WindowShow
	// event arrives asynchronously (Wails emits it over a channel), i.e. AFTER
	// that anchor was computed, and SetSize keeps the existing X/Y and resizes
	// from the TOP-LEFT. So on the first open after a scale change the panel was
	// anchored using the stale size and then grew downward past the taskbar —
	// a 500 DIP panel is 750px at 150%, dropping the bottom edge 250px — which
	// looked like it had jumped towards the middle of the screen. Re-anchoring
	// after the resize closes that race; it is the same call ToggleWindow makes,
	// which is why closing and reopening used to fix it by itself.
	//
	// Wails discards this error at its own call sites; we log it, because a failed
	// anchor silently leaves the panel wherever WM_DPICHANGED put it.
	reanchor := func(*application.WindowEvent) {
		flyout.SetSize(flyoutWidthFor(svc.FlyoutAppearance().PanelWidth), flyoutHeight)
		if err := tray.PositionWindow(flyout, 0); err != nil {
			slog.Debug("could not re-anchor the flyout to the tray", "err", err)
		}
	}
	flyout.OnWindowEvent(events.Common.WindowShow, reanchor)
	flyout.OnWindowEvent(events.Common.WindowDPIChanged, reanchor)

	svc.refreshLicence() // load any installed business licence

	// Re-point any app shortcut whose icon path died with a previous app
	// identity (a re-signing, or a move to the Store build) — taskbar pins in
	// particular, which nothing else ever rewrites. See appshortcuts.go.
	go svc.repairAppShortcutIcons()

	signedIn := hasAccount()
	if !signedIn {
		// First run: show the sign-in window. The engine starts after login.
		// Set directly, not via setStatus: nothing can hear an event yet.
		svc.status = "Not signed in"
		svc.showLogin()
	}
	// The right-click menu exists from launch, not only once an engine is up:
	// with no account the engine never starts, and closing the sign-in window
	// left a tray icon with no menu at all (GitHub #12). Before Run, SetMenu
	// only stores the menu; start() rebuilds it once the engine is running.
	tray.SetMenu(svc.buildTrayMenu())
	if signedIn {
		go svc.start(ctx)
	}
	// One update check for the life of the process, signed in or not. It used
	// to start with the engine, so an app waiting for a sign-in never heard
	// about a release, and the only way to update it was the website (GitHub #11).
	go svc.updateCheckLoop(ctx)

	err := app.Run()
	// Every clean way out passes here (tray Quit, an update, Windows ending the
	// session). A log that stops without this line died another way: see
	// crash.log, or it was killed.
	slog.Info("exiting", "err", err)
	// Give back file locks first: nothing on the server would ever expire them.
	svc.releaseLocksOnExit(5 * time.Second)
	// Disconnect WITHOUT unregistering: unregistering makes Windows strip the
	// cloud state from the whole tree, which is how every app update used to
	// flatten the mount (placeholders reverted to plain files on each restart).
	svc.disconnectAllOnDemand()
	if err != nil {
		slog.Error("application exited with error", "err", err)
		os.Exit(1)
	}
}
