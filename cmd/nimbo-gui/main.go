// Command nimbo-gui is the Nimbo desktop app (Wails v3): a tray icon
// with an attached OneDrive-style flyout panel, backed by the shared sync engine.
package main

import (
	"context"
	"embed"
	"log/slog"
	"os"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"github.com/otherworld/nimbo/internal/account"
	"github.com/otherworld/nimbo/internal/applog"
	"github.com/otherworld/nimbo/internal/brand"
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

// hasAccount reports whether an account is already configured.
func hasAccount() bool {
	d, err := config.Resolve()
	if err != nil {
		return false
	}
	st, err := account.LoadStore(d.AccountsFile())
	if err != nil {
		return false
	}
	_, ok := st.Default()
	return ok
}

// flyoutHeight is the fixed height of the tray flyout panel; its width follows
// the panel-width appearance setting (see flyoutWidthFor).
const flyoutHeight = 500

func main() {
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
	// Re-assert the logical size when the panel is shown or the display scale
	// changes (e.g. a Remote Desktop session switches DPI while it's hidden),
	// otherwise the attached panel can reappear shrunk.
	reassertSize := func(*application.WindowEvent) { flyout.SetSize(flyoutWidthFor(svc.FlyoutAppearance().PanelWidth), flyoutHeight) }
	flyout.OnWindowEvent(events.Common.WindowShow, reassertSize)
	flyout.OnWindowEvent(events.Common.WindowDPIChanged, reassertSize)

	tray := app.SystemTray.New()
	tray.SetIcon(trayIcon("idle", 0, false))
	tray.AttachWindow(flyout)
	svc.tray = tray

	svc.refreshLicence() // load any installed business licence

	if hasAccount() {
		go svc.start(ctx)
	} else {
		// First run: show the sign-in window. The engine starts after login.
		svc.showLogin()
	}

	err := app.Run()
	svc.unmountAllOnDemand() // disconnect on-demand mounts cleanly on exit
	if err != nil {
		slog.Error("application exited with error", "err", err)
		os.Exit(1)
	}
}
