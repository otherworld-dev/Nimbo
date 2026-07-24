//go:build windows

package notify

import (
	"log/slog"
	"net/url"
	"strconv"
	"strings"

	"github.com/gen2brain/beeep"
	"github.com/go-toast/toast"
	"github.com/otherworld/nimbo/internal/transport"
)

// Toast shows a Windows toast with an optional protocol-activation link (clicking
// opens it in the default browser). Falls back to a plain beeep toast if the
// richer toast can't be shown. Safe to call from any goroutine.
func Toast(title, message, link string) {
	if !enabled.Load() {
		return
	}
	// go-toast (and the beeep fallback) interpolate these fields into a
	// double-quoted PowerShell here-string, which PowerShell expands before
	// parsing it as XML — so untrusted notification text could execute code when
	// the toast is merely raised. Neutralise them at this single choke point.
	title = sanitizeToastText(title)
	message = sanitizeToastText(message)
	link = sanitizeToastText(link)
	if title == "" {
		title = "Nimbo"
	}
	// Native in-process WinRT is the real path (no child process, no injection
	// surface). During bring-up the sanitised go-toast path is kept as a fallback
	// for unpackaged/dev runs and any WinRT failure; it will be removed once the
	// native path is proven.
	if err := raiseNativeToast(title, message, link); err == nil {
		slog.Info("toast raised via native WinRT") // TODO: demote to Debug after the spike
		return
	} else if toastAUMID() != "" {
		slog.Warn("native toast raise failed, falling back", "err", err)
	}
	t := toast.Notification{AppID: "Nimbo", Title: title, Message: message}
	if link != "" {
		t.ActivationType = "protocol"
		t.ActivationArguments = link
	}
	if err := t.Push(); err != nil {
		slog.Debug("rich toast failed, falling back to beeep", "err", err)
		_ = beeep.Notify(title, message, "")
	}
}

// sanitizeToastText neutralises a server-supplied string before it reaches
// go-toast. go-toast builds the toast XML inside a DOUBLE-QUOTED PowerShell
// here-string (`$template = @"..."@`) which PowerShell expands — `$(...)`, `$var`
// and backtick escapes — BEFORE parsing it as XML. Untrusted notification text (a
// share note, a filename, a Talk message authored by another user) could
// therefore run arbitrary PowerShell when the toast is raised, with no click. We
// drop the two PowerShell-active characters (`$`, backtick), collapse newlines and
// control characters (a line beginning `"@` would close the here-string), and
// neutralise the `]]>` CDATA break so markup can't be injected either.
//
// This is a focused stopgap. The native in-process WinRT raiser (see
// docs/specs/2026-07-22-actionable-toast-notifications.md) removes the PowerShell
// layer entirely and lets these characters through safely.
func sanitizeToastText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '$' || r == '`':
			// PowerShell expansion / escape triggers — drop.
		case r == '\r' || r == '\n' || r == '\t':
			b.WriteByte(' ')
		case r < 0x20:
			// other control characters — drop.
		default:
			b.WriteRune(r)
		}
	}
	return strings.ReplaceAll(b.String(), "]]>", "]] >")
}

// showToast displays a Nextcloud notification as a toast. Its server-side actions
// (Accept/Decline a share, etc.) become toast buttons that call DoNotificationAction
// via the toast activator; clicking the body opens the in-app notifications tab.
func showToast(n transport.Notification, accountID string) {
	title := n.Subject
	if title == "" {
		title = "Nextcloud"
	}
	acct := "&acct=" + url.QueryEscape(accountID)
	bodyArgs := "action=notifications" + acct + "&id=" + strconv.Itoa(n.ID)
	var buttons []ToastButton
	for _, act := range n.Actions {
		if len(buttons) >= 3 { // Windows truncates beyond ~3 buttons
			break
		}
		buttons = append(buttons, ToastButton{
			Label: act.Label,
			Args:  "action=notify" + acct + "&id=" + strconv.Itoa(n.ID) + "&label=" + url.QueryEscape(act.Label),
		})
	}
	RaiseActionable(title, strings.TrimSpace(n.Message), bodyArgs, buttons)
}
