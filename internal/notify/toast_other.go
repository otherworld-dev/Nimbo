//go:build !windows

package notify

import (
	"log/slog"
	"strings"

	"github.com/gen2brain/beeep"
	"github.com/otherworld/nimbo/internal/transport"
)

// Toast shows a native toast via beeep. Clickable activation is Windows-specific;
// elsewhere the GUI panel provides click-through, so link is ignored.
func Toast(title, message, _ string) {
	if !enabled.Load() {
		return
	}
	if title == "" {
		title = "Nimbo"
	}
	if err := beeep.Notify(title, message, ""); err != nil {
		slog.Warn("toast failed", "err", err)
	}
}

// RaiseActionable falls back to a plain toast off-Windows (activation is
// Windows-specific; the GUI panel provides click-through elsewhere).
func RaiseActionable(title, message, _ string, _ []ToastButton) {
	Toast(title, message, "")
}

// showToast displays a Nextcloud notification as a toast. accountID is unused
// off-Windows (no clickable activation); the GUI panel provides click-through.
func showToast(n transport.Notification, _ string) {
	title := n.Subject
	if title == "" {
		title = "Nextcloud"
	}
	Toast(title, strings.TrimSpace(n.Message), n.Link)
}
