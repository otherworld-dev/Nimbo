// Package notify turns Nextcloud notifications into native desktop toasts and
// keeps a live, subscribable view of the current notifications for UIs. It
// tracks which IDs have been toasted so the same item is never shown twice.
package notify

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/otherworld/nimbo/internal/transport"
)

// enabled gates all desktop toasts (both Nextcloud notifications and the
// app's own conflict/error toasts). Default on; the GUI flips it from settings.
var enabled atomic.Bool

func init() { enabled.Store(true) }

// SetEnabled turns desktop toasts on or off globally.
func SetEnabled(on bool) { enabled.Store(on) }

// ToastButton is an action button on a toast. Label is shown to the user; Args is
// the activation string (a URL query like "action=login") routed to the app's
// toast-activation handler when the button is clicked.
type ToastButton struct {
	Label string
	Args  string
}

// Enabled reports whether desktop toasts are currently allowed.
func Enabled() bool { return enabled.Load() }

// Notifier fetches notifications, toasts new ones, and exposes the current set.
type Notifier struct {
	client    *transport.Client
	accountID string // owning account — carried into toast args so a click acts on the right account

	mu      sync.Mutex
	seen    map[int]bool
	current []transport.Notification
	subs    []chan struct{}
}

// New creates a Notifier for the given account client. accountID identifies the
// account so a toast's Accept/Decline routes back to it (multi-account setups).
func New(client *transport.Client, accountID string) *Notifier {
	return &Notifier{client: client, accountID: accountID, seen: make(map[int]bool)}
}

// Prime records the currently-pending notifications without toasting them (so a
// freshly started client doesn't replay a backlog) and populates the list.
func (n *Notifier) Prime(ctx context.Context) error {
	_, err := n.refresh(ctx, false)
	return err
}

// Check fetches notifications, toasts any not seen before, and returns the count
// of new ones.
func (n *Notifier) Check(ctx context.Context) (int, error) {
	return n.refresh(ctx, true)
}

// Refresh re-fetches the list without toasting (e.g. after a dismiss/action).
func (n *Notifier) Refresh(ctx context.Context) error {
	_, err := n.refresh(ctx, false)
	return err
}

func (n *Notifier) refresh(ctx context.Context, toast bool) (int, error) {
	items, err := n.client.Notifications(ctx)
	if err != nil {
		return 0, err
	}

	n.mu.Lock()
	newCount := 0
	for _, it := range items {
		if !n.seen[it.ID] {
			if toast {
				showToast(it, n.accountID)
				slog.Info("notification", "app", it.App, "subject", it.Subject)
			}
			newCount++
		}
		n.seen[it.ID] = true
	}
	n.current = items
	subs := append([]chan struct{}(nil), n.subs...)
	n.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	return newCount, nil
}

// List returns the most recently fetched notifications.
func (n *Notifier) List() []transport.Notification {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]transport.Notification, len(n.current))
	copy(out, n.current)
	return out
}

// Count returns the number of current notifications.
func (n *Notifier) Count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.current)
}

// Subscribe returns a channel signalled whenever the notification set changes.
func (n *Notifier) Subscribe() <-chan struct{} {
	ch := make(chan struct{}, 1)
	n.mu.Lock()
	n.subs = append(n.subs, ch)
	n.mu.Unlock()
	return ch
}
