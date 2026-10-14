package main

// Signing in again after the server turns an account's credentials down (an
// app password revoked in the web UI, GitHub #20). Two things the reporter
// found unfriendly:
//
//   - The shown account losing its sign-in stopped every account, so the app
//     read as signed out and the others couldn't be reached. Now another
//     account becomes the shown one and everything that can sync carries on.
//   - Signing in again looked exactly like adding a new account: a blank server
//     box, then the whole setup screen. Now the sign-in window names the
//     account, says its folders and settings are kept, and an account Nimbo
//     already has skips setup.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/otherworld/nimbo/internal/account"
	"github.com/otherworld/nimbo/internal/config"
	"github.com/otherworld/nimbo/internal/notify"
	"github.com/otherworld/nimbo/internal/transport"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// SignInTargetDTO is the account the sign-in window asks to sign in again.
// An empty ID means none in particular: the window signs in a new account.
type SignInTargetDTO struct {
	ID      string `json:"id"`
	User    string `json:"user"`
	Server  string `json:"server"`
	Address string `json:"address"` // "login@host"
}

// SignInTarget tells the sign-in window which account it is for: the one it was
// opened for, or with nothing running because the shown account was turned
// down, that account.
func (a *App) SignInTarget() SignInTargetDTO {
	a.acctMu.Lock()
	id, other := a.signInFor, a.signInOther
	a.acctMu.Unlock()
	if other {
		return SignInTargetDTO{}
	}
	_, st, ok := accountsAndDirs()
	if !ok {
		return SignInTargetDTO{}
	}
	if id == "" && a.eng == nil && a.authLost.Load() {
		if def, ok := st.Default(); ok {
			id = def.ID
		}
	}
	ac, found := st.Find(id)
	if id == "" || !found {
		return SignInTargetDTO{}
	}
	return SignInTargetDTO{ID: ac.ID, User: ac.LoginName, Server: ac.ServerURL, Address: ac.Address()}
}

// SignInAgain opens the sign-in window for one account (its row in Settings).
func (a *App) SignInAgain(id string) { a.showLoginFor(id) }

// SignInSomeoneElse is the sign-in window's "Sign in to a different account
// instead": until the window closes, it signs in whichever account the user
// picks rather than the one it was opened for.
func (a *App) SignInSomeoneElse() {
	a.acctMu.Lock()
	a.signInOther = true
	a.acctMu.Unlock()
}

// showLoginFor opens the sign-in window asking account id to sign in again
// ("" = a new account, or whichever account needs it).
func (a *App) showLoginFor(id string) {
	a.setSignInFor(id)
	a.showLogin()
	a.emit("login:target") // an open window re-reads its target
}

// setShowOnSignIn records the account to show once it has signed in again
// ("" = none): signing in a background account otherwise leaves the shown one
// as it is.
func (a *App) setShowOnSignIn(id string) {
	a.acctMu.Lock()
	a.showOnSignIn = id
	a.acctMu.Unlock()
}

func (a *App) setSignInFor(id string) {
	a.acctMu.Lock()
	a.signInFor, a.signInOther = id, false
	a.acctMu.Unlock()
}

// raiseLogin brings the sign-in window back and in front of whatever is in
// use. A topmost round trip puts it above other windows even when Windows
// won't hand Nimbo the focus.
func (a *App) raiseLogin() {
	w := a.loginWin
	if w == nil {
		return
	}
	w.UnMinimise()
	w.Show()
	w.SetAlwaysOnTop(true)
	w.SetAlwaysOnTop(false)
	w.Focus()
}

// loginStepAside minimises the sign-in window while the browser sign-in runs.
// Left in place it sat over the browser's Grant access button, and closing it
// to get at the button cancelled the sign-in (GitHub #20 testing). It comes
// back for setup or an error; an account signing in again just closes it.
func (a *App) loginStepAside() {
	if w := a.loginWin; w != nil {
		w.Minimise()
	}
}

// loginFailed reports a failed browser sign-in in the sign-in window, brought
// back so the message is seen.
func (a *App) loginFailed(msg string) {
	a.raiseLogin()
	a.app.Event.Emit("login:error", msg)
}

// wrongAccount reports whether a finished sign-in is for some other account
// than the one the window asked for (want, "" = any). The browser decides who
// signs in: one still logged in to Nextcloud as someone else grants for them.
func wrongAccount(want string, got account.Account, known bool) bool {
	return want != "" && (!known || got.ID != want)
}

// signInArgs is the toast activation that opens the sign-in window for id.
func signInArgs(id string) string {
	return "action=login&acct=" + url.QueryEscape(id)
}

// isSignInRejected reports whether an engine failed to start because the
// account's credentials are gone or the server turned them down, which only
// signing in again fixes.
func isSignInRejected(err error) bool {
	return errors.Is(err, account.ErrNoSecret) || errors.Is(err, transport.ErrUnauthorized)
}

// markAuthLost records that the server turned an account's credentials down.
// It is not started again until it signs in, and its row in Settings says so.
func (a *App) markAuthLost(id string) {
	a.acctMu.Lock()
	if a.authLostIDs == nil {
		a.authLostIDs = map[string]bool{}
	}
	a.authLostIDs[id] = true
	if a.acctStatus == nil {
		a.acctStatus = map[string]string{}
	}
	a.acctStatus[id] = "Sign in again"
	a.acctMu.Unlock()
	a.emit("account")
}

func (a *App) clearAuthLost(id string) {
	a.acctMu.Lock()
	delete(a.authLostIDs, id)
	if a.acctStatus[id] == "Sign in again" {
		delete(a.acctStatus, id)
	}
	a.acctMu.Unlock()
}

func (a *App) isAuthLost(id string) bool {
	a.acctMu.Lock()
	defer a.acctMu.Unlock()
	return a.authLostIDs[id]
}

// pickFallback chooses the account to show when the shown one has lost its
// sign-in: the first that hasn't lost its own and still has a password. None
// means every account needs signing in, which is the signed-out state.
func pickFallback(accounts []account.Account, lost, hasSecret func(id string) bool) (account.Account, bool) {
	for _, ac := range accounts {
		if !lost(ac.ID) && hasSecret(ac.ID) {
			return ac, true
		}
	}
	return account.Account{}, false
}

func (a *App) fallbackAccount() (account.Account, bool) {
	_, st, ok := accountsAndDirs()
	if !ok {
		return account.Account{}, false
	}
	return pickFallback(st.Accounts, a.isAuthLost, func(id string) bool {
		_, err := account.LoadSecret(id)
		return err == nil
	})
}

func defaultAccount() (account.Account, bool) {
	_, st, ok := accountsAndDirs()
	if !ok {
		return account.Account{}, false
	}
	return st.Default()
}

// carryOnWith shows next in place of lost, the shown account whose sign-in was
// turned down, and asks lost to sign in again. Called on the UI thread with
// the engines already stopped; the restart runs off it, since starting an
// engine talks to the server.
func (a *App) carryOnWith(lost, next account.Account) {
	a.setShowOnSignIn(lost.ID)
	slog.Warn("shown account can't sign in; showing another", "account", lost.LoginName, "now", next.LoginName)
	notify.RaiseActionable("Sign in required", lost.Address()+" needs signing in again. Your other accounts carry on syncing.",
		signInArgs(lost.ID), []notify.ToastButton{{Label: "Sign in", Args: signInArgs(lost.ID)}})
	a.showLoginFor(lost.ID)
	go func() {
		d, err := config.Resolve()
		if err == nil {
			err = account.Update(d.AccountsFile(), func(s *account.Store) error { return s.SetDefault(next.ID) })
		}
		if err != nil {
			// Starting now would bind to the account that just failed again.
			slog.Warn("could not show another account", "err", err)
			application.InvokeAsync(func() {
				a.authLost.Store(true)
				a.setStatus("Sign in again")
				a.rebuildTrayMenu()
			})
			return
		}
		a.start(a.ctx)
		a.emit("account")
		a.rebuildTrayMenu()
	}()
}

// completeSignIn saves a finished browser sign-in and restarts what it
// affects. An account Nimbo already has is signing in again (again = true): it
// keeps its folders and settings, so setup is skipped, and it becomes the
// shown account only if it was the shown one before it lost its sign-in, was
// picked in the account switcher, or nothing else is running.
// Runs on the sign-in poll's goroutine.
func (a *App) completeSignIn(creds account.Credentials, want SignInTargetDTO) (again bool, err error) {
	d, err := config.Resolve()
	if err != nil {
		return false, err
	}
	st, err := account.LoadStore(d.AccountsFile())
	if err != nil {
		return false, err
	}
	// Matched ignoring case: Nextcloud takes the login in any case and hands
	// back what was typed, and "test account 2" is still "Test Account 2".
	prev, again := st.FindLogin(creds.Server, creds.LoginName)
	id := prev.ID
	if wrongAccount(want.ID, prev, again) {
		// Saved, it would sign in an account nobody asked for here and leave
		// the one asked for signed out. Give the server back its new app
		// password, which nothing will use.
		go revokeAppPassword(creds)
		got := account.Account{ServerURL: creds.Server, LoginName: creds.LoginName}.Address()
		return false, fmt.Errorf("your browser signed in to Nextcloud as %s, not %s. Log out of Nextcloud in your browser, then sign in again.", got, want.Address)
	}
	if !again {
		acct, err := account.Complete(d.AccountsFile(), creds)
		if err != nil {
			return false, err
		}
		// The setup screen comes next and chooses the account's folder, so the
		// start below must not mount a default one ahead of it (GitHub #11).
		a.beginSetup(acct.ID)
		a.restartShown()
		return false, nil
	}

	a.acctMu.Lock()
	show := a.showOnSignIn == id
	a.acctMu.Unlock()
	if def, ok := st.Default(); ok && def.ID == id {
		show = true
	}
	if a.eng == nil {
		show = true // nothing else is running, so this is the account to show
	}
	if show {
		if _, err := account.Complete(d.AccountsFile(), creds); err != nil {
			return true, err
		}
		a.acctMu.Lock()
		if a.showOnSignIn == id {
			a.showOnSignIn = ""
		}
		a.acctMu.Unlock()
		a.clearAuthLost(id)
		a.restartShown()
		return true, nil
	}
	if _, err := account.Reauthorize(d.AccountsFile(), creds); err != nil {
		return true, err
	}
	a.clearAuthLost(id)
	a.stopSecondary(id) // a running one starts again on the new password
	a.startSecondaries(a.ctx)
	a.emit("account")
	return true, nil
}

// restartShown restarts syncing on the shown account from the store, the way
// a fresh sign-in always has.
func (a *App) restartShown() {
	if a.eng != nil {
		a.disconnectAllOnDemand()
		a.stopEngine()
	}
	a.start(a.ctx)
}

// revokeAppPassword deletes an app password a sign-in produced but Nimbo isn't
// keeping, so it doesn't linger in the user's Devices & sessions list.
func revokeAppPassword(creds account.Credentials) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := transport.New(creds.Server, creds.LoginName, creds.AppPassword)
	if err := c.DeleteAppPassword(ctx); err != nil {
		slog.Warn("could not revoke an unused app password", "login", creds.LoginName, "err", err)
	}
}
