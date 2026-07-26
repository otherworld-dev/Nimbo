package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"github.com/otherworld/nimbo/internal/brand"
	"github.com/otherworld/nimbo/internal/config"
	"github.com/otherworld/nimbo/internal/notify"
)

// App windows: a Nextcloud web app opened as its own desktop window — its own
// taskbar identity (per-window AppUserModelID + icon on Windows), singleton per
// app, loading the app's URL straight off the user's server. The webview shares
// one WebView2 profile across all windows, so the Nextcloud web session is
// entered once (first window shows the login) and persists across launches;
// the sync engine's own app-password auth is unrelated.
//
// Threading: openAppWindow may be called from binding goroutines (dock clicks),
// plain goroutines (--app launches) and window-event goroutines — appWinsMu
// guards the window map, held only for short in-memory sections (never across
// InvokeSync/network, which can deadlock against the main thread).

const (
	appWindowDefaultW = 1100
	appWindowDefaultH = 760

	// appWindowPrefix names an app window. Also the gate on raw page messages:
	// only these windows host a remote (server-controlled) page, so only they
	// speak the external-link protocol below.
	appWindowPrefix = "app:"

	// externalLinkPrefix tags the raw message an app window's page sends to
	// hand a link to the user's real browser. Raw messages — anything the
	// webview posts that Wails doesn't claim with its own "wails:" prefix —
	// arrive via application.Options.RawMessageHandler, a channel that needs no
	// bound method, so this can't disturb the generated bindings.
	externalLinkPrefix = "nimbo:openExternal:"

	// maxExternalLinkLen bounds what we'll hand to the OS launcher. Real links
	// are far shorter; anything past this is malformed or hostile.
	maxExternalLinkLen = 8192
)

// loginNudgeJS builds the script injected on every app-window navigation. On
// the Nextcloud login page it ticks "remember me" (id #rememberme in the Vue
// login — a plain session cookie doesn't survive a WebView2 restart, the
// remember-me token does) and prefills the username. The fields render after
// Vue mounts (post-navigation), so it polls briefly; each field is set once, on
// first appearance, only if still empty — so it never overwrites what the user
// types. Off the login page the selectors match nothing: an inert no-op.
func loginNudgeJS(loginName string) string {
	u, _ := json.Marshal(loginName)
	return fmt.Sprintf(`(function(){
if(window.__nimboLoginNudge)return; window.__nimboLoginNudge=1;
var n=0,du=false,dr=false,USER=%s;
var t=setInterval(function(){
 if(++n>75){clearInterval(t);return;}
 try{
  if(!/(^|\/)login(\/|$|\?)/.test(location.pathname)){clearInterval(t);return;}
  if(!dr){var r=document.querySelector('#rememberme,input[name="rememberme"]');
   if(r){if(!r.checked){r.checked=true;r.dispatchEvent(new Event('change',{bubbles:true}));}dr=true;}}
  if(!du){var u=document.querySelector('#user,input[name="user"],input[autocomplete="username"]');
   if(u){if(!u.value){u.value=USER;u.dispatchEvent(new Event('input',{bubbles:true}));}du=true;}}
  if(du&&dr){clearInterval(t);}
 }catch(e){clearInterval(t);}
},200);
})();`, string(u))
}

// appWindowChromeJS hides Nextcloud's global header bar inside app windows —
// the window IS the app, so the server's app-switcher/search/avatar bar just
// duplicates the dock. A persistent <style> keeps it hidden across the SPA's
// client-side routing; NC lays content out off --header-height, so zeroing the
// variable lets the server's own calc() reflow the page. The splash and login
// pages have no #header, so it's inert there. Browser opens (dock right-click)
// keep the full Nextcloud chrome.
const appWindowChromeJS = `(function(){try{
if(document.getElementById('nimbo-appwin-css'))return;
var s=document.createElement('style');s.id='nimbo-appwin-css';
s.textContent='#header{display:none!important}body,:root{--header-height:0px!important}#content{margin-top:0!important;margin-block-start:0!important}';
(document.head||document.documentElement).appendChild(s);
}catch(e){}})();`

// externalLinkJS builds the script that keeps other people's websites out of
// Nimbo. Nextcloud apps link outwards with target="_blank" (the Bookmarks app
// is all such links); WebView2 answers that with NewWindowRequested, and Wails
// v3 never subscribes to it, so the webview falls back to its own default and
// opens the site in a bare popup owned by us — no address bar, none of the
// user's browser. There's no Go-side hook for that event, so we get in first:
// catch the click in the page, cancel it, and post the URL back for the system
// browser. Off-origin http(s) only — links that stay on the user's own server
// still belong in the app window.
//
// The listeners are capture-phase and on `document`, so they survive
// Nextcloud's client-side routing and only need installing once per real
// navigation. Re-running the script is a no-op (see the __nimboExtLinks guard).
func externalLinkJS(homeOrigin string) string {
	h, _ := json.Marshal(homeOrigin)
	p, _ := json.Marshal(externalLinkPrefix)
	return fmt.Sprintf(`(function(){
if(window.__nimboExtLinks)return; window.__nimboExtLinks=1;
var HOME=%s,PREFIX=%s;
// ext(): the absolute URL to hand off, or null to leave the click alone.
function ext(u){try{
 if(!u)return null;
 var a=new URL(u,location.href);
 if(a.protocol!=='http:'&&a.protocol!=='https:')return null;
 if(a.origin===location.origin)return null;
 if(HOME&&a.origin===HOME)return null;
 return a.href;
}catch(e){return null;}}
function send(u){try{window.chrome.webview.postMessage(PREFIX+u);return true;}catch(e){return false;}}
// The clicked anchor, through shadow roots where the browser exposes them.
function anchor(e){
 var p=(e.composedPath&&e.composedPath())||[];
 for(var i=0;i<p.length;i++){var n=p[i];if(n&&n.tagName==='A'&&n.getAttribute)return n;}
 var t=e.target;
 while(t&&t.nodeType===1){if(t.tagName==='A'&&t.getAttribute)return t;t=t.parentElement||t.parentNode;}
 return null;
}
// button 0 = left (incl. ctrl/shift-click), 1 = middle. Middle arrives as
// auxclick in Chromium, never as click, so both events share this handler.
function onClick(e){
 if(e.defaultPrevented||(e.button!==0&&e.button!==1))return;
 var a=anchor(e); if(!a)return;
 var u=ext(a.getAttribute('href')); if(!u)return;
 if(!send(u))return;              // no channel: let the page do its thing
 e.preventDefault(); e.stopPropagation();
}
document.addEventListener('click',onClick,true);
document.addEventListener('auxclick',onClick,true);
var open=window.open;
window.open=function(u){var x=ext(u); if(x&&send(x))return null; return open.apply(window,arguments);};
})();`, string(h), string(p))
}

// serverOrigin is the scheme://host of the signed-in Nextcloud — what app
// windows measure a link against to decide "still the app" vs "the web".
// Empty when signed out, which just leaves the page's own origin as the test.
func (a *App) serverOrigin() string {
	if a.eng == nil {
		return ""
	}
	u, err := url.Parse(strings.TrimRight(a.eng.Account.ServerURL, "/"))
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// handleRawMessage receives postMessage traffic from a window's page that
// isn't part of the Wails protocol. Wired as application.Options.RawMessageHandler.
func (a *App) handleRawMessage(w application.Window, message string, _ *application.OriginInfo) {
	if w == nil {
		return
	}
	if href, ok := parseExternalLink(w.Name(), message); ok {
		openURL(href)
	}
}

// parseExternalLink validates an external-link request before it reaches the
// OS launcher. The sender is a page served by the user's Nextcloud, so the
// message is untrusted input: it must come from an app window, carry the
// agreed prefix, and name an absolute http(s) URL. Anything else is dropped —
// notably other schemes, which would let a crafted page pick the handler
// (file:, ms-settings:, …) rather than just the browser.
func parseExternalLink(windowName, message string) (string, bool) {
	if !strings.HasPrefix(windowName, appWindowPrefix) {
		return "", false
	}
	raw, ok := strings.CutPrefix(message, externalLinkPrefix)
	if !ok || raw == "" || len(raw) > maxExternalLinkLen {
		return "", false
	}
	u, err := url.Parse(raw) // rejects control characters
	if err != nil || u.Host == "" {
		return "", false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false
	}
	return u.String(), true
}

// aumidFor derives the per-app-window AppUserModelID. Deliberately NOT the
// package AUMID (PFN!App) — the whole point is a distinct taskbar identity per
// Nextcloud app; any string ≤128 chars works, and the Start-menu shortcut
// carries the same one so pinned buttons and windows group correctly.
// brand-derived so white-label builds get their own prefix.
func aumidFor(id string) string { return brand.Current.AppID + ".App." + id }

// appIDRe is the shape of a trustworthy Nextcloud app id. The id comes from the
// server's navigation API and ends up in a Start-menu shortcut's argv
// ("--app <id>") — without this gate a hostile server could smuggle extra
// flags (argument injection) or path/AUMID junk through a crafted id. Real
// Nextcloud app ids are lowercase [a-z0-9_]; allow case/dot/hyphen for slack.
var appIDRe = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,64}$`)

// openAppWindow opens (or focuses) the dedicated window for a Nextcloud app id.
// Safe to call from any goroutine; blocks on a bounded app-list fetch, so don't
// call it from the UI thread.
func (a *App) openAppWindow(id string) {
	id = strings.TrimSpace(strings.Trim(id, "/"))
	if !appIDRe.MatchString(id) {
		return
	}
	if a.eng == nil {
		notify.Toast(brand.Current.Name, "Sign in first — apps open from your Nextcloud account.", "")
		return
	}

	// Focus an existing window, or reserve the id so a double-click during the
	// (network-slow) app-list fetch can't open two windows.
	a.appWinsMu.Lock()
	if w := a.appWins[id]; w != nil {
		a.appWinsMu.Unlock()
		bringToFront(w)
		return
	}
	if a.appOpening[id] {
		a.appWinsMu.Unlock()
		return // an open for this app is already in flight
	}
	if a.appOpening == nil {
		a.appOpening = map[string]bool{}
	}
	a.appOpening[id] = true
	a.appWinsMu.Unlock()
	defer func() {
		a.appWinsMu.Lock()
		delete(a.appOpening, id)
		a.appWinsMu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	apps, err := a.cachedApps(ctx) // warm from the flyout — usually instant
	if err != nil {
		notify.Toast(brand.Current.Name, "Couldn't load the app list from your server.", "")
		return
	}
	name, href, icon := "", "", ""
	for _, ap := range apps {
		if ap.ID == id {
			name, href, icon = ap.Name, ap.Href, ap.Icon
			break
		}
	}
	if href == "" {
		notify.Toast(brand.Current.Name, "App "+id+" isn't available on your server.", "")
		return
	}
	if name == "" {
		name = id
	}

	w, h := a.appWindowSize(id)
	// The window starts on an embedded splash (instant, branded, shows the app
	// name/icon) and is redirected to the real app once that first navigation
	// completes — the splash then stays visible while the server renders, so
	// the user never stares at a white window during a slow load.
	splash := "/#appsplash/" + url.PathEscape(name) + "?icon=" + url.QueryEscape(a.absURL(icon))
	// Hidden + default StartState is load-bearing: the HWND exists when
	// NewWithOptions returns and the window can't auto-show before the taskbar
	// identity (AUMID + icon) is applied — so it never appears mis-grouped.
	win := a.app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:   appWindowPrefix + id,
		Title:  name,
		Width:  w,
		Height: h,
		Hidden: true,
		URL:    splash,
	})
	ico := a.appIconPath(id)
	application.InvokeSync(func() { // shell property store wants the UI (STA) thread
		setAppWindowIdentity(win.NativeWindow(), id, aumidFor(id), ico)
	})
	a.appWinsMu.Lock()
	if a.appWins == nil {
		a.appWins = map[string]*application.WebviewWindow{}
	}
	a.appWins[id] = win
	a.appWinsMu.Unlock()
	// A hook, not a listener: hooks run synchronously before Wails' own closing
	// listener destroys the window, so Size() still reads the live window.
	win.RegisterHook(events.Common.WindowClosing, func(*application.WindowEvent) {
		cw, ch := win.Size()
		go a.saveAppWindowSize(id, cw, ch) // disk write off the UI thread
		a.appWinsMu.Lock()
		if a.appWins[id] == win {
			delete(a.appWins, id)
		}
		a.appWinsMu.Unlock()
	})
	// Per navigation: (1) once the SPLASH has rendered, redirect to the real
	// app — the splash stays on screen until the app's page commits, covering
	// the whole server wait; (2) on the Nextcloud login page, nudge toward a
	// lasting session — tick "remember me" (plain session cookies don't survive
	// a WebView2 restart; the remember-me token does) and prefill the username.
	// The nudge is an inert no-op everywhere else. (3) links that point off the
	// user's server are sent to their real browser instead of opening in here.
	nudge := loginNudgeJS(a.eng.Account.LoginName)
	extLinks := externalLinkJS(a.serverOrigin())
	target := a.absURL(href)
	var toApp sync.Once
	win.OnWindowEvent(events.Windows.WebViewNavigationCompleted, func(*application.WindowEvent) {
		toApp.Do(func() { win.SetURL(target) })
		win.ExecJS(appWindowChromeJS) // desktop-app feel: no NC header inside the window
		win.ExecJS(nudge)
		win.ExecJS(extLinks)
	})
	bringToFront(win)
	if a.flyout != nil {
		a.flyout.Hide()
	}
	// Icon + Start-menu entry in the background. The shortcut is what makes
	// "pin to taskbar" on this window work properly — Windows backs the pin
	// with the .lnk that carries the same AUMID (icon + relaunch); without it
	// a pin shows the bare exe icon and relaunches into nothing. The icon is
	// then re-applied to the live window: it may have opened before the .ico
	// existed (first open / icon-style migration) with only the brand fallback.
	go func() {
		a.ensureAppIcon(id, true)
		a.appWinsMu.Lock()
		cur := a.appWins[id]
		a.appWinsMu.Unlock()
		if cur == win {
			application.InvokeSync(func() {
				refreshWindowIcon(win.NativeWindow(), id, ico)
			})
		}
		a.autoCreateAppShortcut(id, name)
		// The icon may only just have landed; let any taskbar pin catch up too
		// (nothing else rewrites a pin). Cheap and idempotent when there's
		// nothing to fix.
		a.repairAppShortcutIcons()
	}()
}

// autoCreateAppShortcut ensures a Start-menu shortcut exists for an app the
// user has opened as a window (PWA-style "installed" feel; the Pin-apps
// editor's toggle removes it, which opts the app out of auto-creation). An
// EXISTING shortcut is silently re-written too: its IconLocation must track
// the rev-versioned icon path, or an icon-style migration would strand it on
// a stale (possibly deleted) file. NOTE: a taskbar pin is Windows' own COPY of
// the .lnk, so refreshing the Start entry does nothing for it — that's what
// repairAppShortcutIcons is for, called just after this.
func (a *App) autoCreateAppShortcut(id, name string) {
	d, err := config.Resolve()
	if err != nil {
		return
	}
	s, err := d.LoadSettings()
	if err != nil || s.AppShortcutsOptOut[id] {
		return
	}
	oldName := shortcutFileFor(s, id, name)
	curName := sanitizeFileName(name) + ".lnk"
	existed := shortcutExists(oldName)
	if err := createAppShortcut(id, name, aumidFor(id), a.appIconPath(id)); err != nil {
		slog.Debug("could not auto-create app shortcut", "app", id, "err", err)
		return
	}
	if existed && oldName != curName {
		_ = removeShortcutFile(oldName) // app was renamed server-side — retire the old entry
	}
	if s, err = d.LoadSettings(); err != nil { // reload: minimize clobbering concurrent writers
		return
	}
	if s.AppShortcuts == nil {
		s.AppShortcuts = map[string]string{}
	}
	s.AppShortcuts[id] = curName
	_ = d.SaveSettings(s)
	if !existed {
		notify.Toast(name+" added to the Start menu", "Pin it to the taskbar if you like — remove it via the dock's Pin-apps editor.", "")
		a.emit("apps")
	}
}

// appWindowSize returns the app's remembered window size, or the default.
func (a *App) appWindowSize(id string) (int, int) {
	if d, err := config.Resolve(); err == nil {
		if s, err := d.LoadSettings(); err == nil {
			if sz, ok := s.AppWindowSizes[id]; ok && sz.W >= 400 && sz.H >= 300 {
				return sz.W, sz.H
			}
		}
	}
	return appWindowDefaultW, appWindowDefaultH
}

// saveAppWindowSize remembers an app window's size for its next open.
func (a *App) saveAppWindowSize(id string, w, h int) {
	if w < 400 || h < 300 {
		return // minimised/degenerate — don't remember it
	}
	d, err := config.Resolve()
	if err != nil {
		return
	}
	s, err := d.LoadSettings()
	if err != nil {
		return
	}
	if s.AppWindowSizes == nil {
		s.AppWindowSizes = map[string]config.AppWindowSize{}
	}
	if cur, ok := s.AppWindowSizes[id]; ok && cur.W == w && cur.H == h {
		return
	}
	s.AppWindowSizes[id] = config.AppWindowSize{W: w, H: h}
	if err := d.SaveSettings(s); err != nil {
		slog.Debug("could not save app window size", "app", id, "err", err)
	}
}

// shortcutFileFor resolves the .lnk filename for an app: the recorded one if a
// shortcut was created (id-keyed, so a server-side display-name change can't
// orphan it), else the filename a new shortcut would get.
func shortcutFileFor(s config.Settings, id, name string) string {
	if f := s.AppShortcuts[id]; f != "" {
		return f
	}
	return sanitizeFileName(name) + ".lnk"
}

// toggleAppShortcut creates or removes the Start-menu shortcut for an app
// (Windows only; a no-op toast elsewhere).
func (a *App) toggleAppShortcut(id string) {
	if !appIDRe.MatchString(id) {
		return // never let an unvetted id near a shortcut's argv (see appIDRe)
	}
	if a.eng == nil {
		notify.Toast(brand.Current.Name, "Sign in first.", "")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	apps, err := a.cachedApps(ctx)
	if err != nil {
		notify.Toast(brand.Current.Name, "Couldn't load the app list from your server.", "")
		return
	}
	name := ""
	for _, ap := range apps {
		if ap.ID == id {
			name = ap.Name
			break
		}
	}
	if name == "" {
		name = id
	}
	d, err := config.Resolve()
	if err != nil {
		return
	}
	s, err := d.LoadSettings()
	if err != nil {
		return
	}
	fname := shortcutFileFor(s, id, name)

	if shortcutExists(fname) {
		if err := removeShortcutFile(fname); err != nil {
			notify.Toast(brand.Current.Name, "Couldn't remove the Start menu shortcut: "+err.Error(), "")
			return
		}
		delete(s.AppShortcuts, id)
		if s.AppShortcutsOptOut == nil {
			s.AppShortcutsOptOut = map[string]bool{}
		}
		s.AppShortcutsOptOut[id] = true // don't auto-recreate on the next window open
		_ = d.SaveSettings(s)
		notify.Toast(brand.Current.Name, name+" removed from the Start menu.", "")
		a.emit("apps")
		return
	}
	a.ensureAppIcon(id, true) // shortcut points at the ico — fetch before creating
	if err := createAppShortcut(id, name, aumidFor(id), a.appIconPath(id)); err != nil {
		notify.Toast(brand.Current.Name, "Couldn't add to the Start menu: "+err.Error(), "")
		return
	}
	if s.AppShortcuts == nil {
		s.AppShortcuts = map[string]string{}
	}
	s.AppShortcuts[id] = sanitizeFileName(name) + ".lnk"
	delete(s.AppShortcutsOptOut, id) // an explicit add re-enables auto-care
	_ = d.SaveSettings(s)
	notify.Toast(name+" added to the Start menu", "Launch or pin it like any app — it opens in its own window.", "")
	a.emit("apps")
}
