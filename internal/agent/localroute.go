package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/otherworld/nimbo/internal/account"
	"github.com/otherworld/nimbo/internal/transport"
)

// Local network route — docs/specs/2026-09-13-local-network-route-design.md.
// The transport does the routing; this file owns WHEN the local address is
// re-tried and everything setup needs from a running engine.

// routeProbeEvery is how often the local address is re-tried while on the
// public route (a var so tests can shorten it).
var routeProbeEvery = time.Minute

// ErrDifferentServer: the local address answers, but with another instance or
// another user's home. The GUI keys on it for a specific message.
var ErrDifferentServer = errors.New("the local address answers, but it is a different Nextcloud server or account")

// routeSwitchMessage is the one log line per route change.
func routeSwitchMessage(route, reason, addr string) string {
	if route == transport.RouteLocal {
		return "sync route: local (" + addr + ")"
	}
	return "sync route: public (" + reason + ")"
}

// routeLoop keeps the local route honest: while on public it probes every
// routeProbeEvery and straight after an up→down flip; while on local it does
// nothing — real traffic failing is the signal. Logs each switch once. Runs
// even when no local route is configured (the tick is free) so a route added
// later via ApplyLocalRoute is picked up without restarting the engine.
func (e *Engine) routeLoop(ctx context.Context) {
	last, _ := e.client.Route()
	t := time.NewTicker(routeProbeEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-e.client.RouteDown():
		}
		if !e.client.HasLocalRoute() {
			continue
		}
		if route, _ := e.client.Route(); route == transport.RouteLocal {
			last = route
			continue
		}
		_ = e.client.ProbeLocal(ctx)
		route, reason := e.client.Route()
		if route != last {
			slog.Info(routeSwitchMessage(route, reason, e.client.LocalAddress()))
			last = route
		}
	}
}

// Route reports the sync client's current route and, on public, why the local
// address is not in use.
func (e *Engine) Route() (route, reason string) { return e.client.Route() }

// CheckLocalRoute verifies addr really serves this account: the DAV root's
// oc:id fetched over the public URL must equal the one fetched over addr. It
// uses a throwaway client so the running route is untouched, and returns the
// id to store as LocalRoute.RootID. addr is host:port; pin "" = OS trust.
func (e *Engine) CheckLocalRoute(ctx context.Context, addr, pin string) (string, error) {
	c := transport.New(e.Account.ServerURL, e.Account.LoginName, e.secret)
	if err := c.SetLocalRoute(addr, pin, ""); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	pub, err := c.RootID(transport.ForceRoute(ctx, transport.RoutePublic))
	if err != nil {
		return "", fmt.Errorf("could not reach the server over the internet to compare (needed once, at setup): %w", err)
	}
	loc, err := c.RootID(transport.ForceRoute(ctx, transport.RouteLocal))
	if err != nil {
		return "", fmt.Errorf("local address: %w", err)
	}
	if pub != loc {
		return "", ErrDifferentServer
	}
	return pub, nil
}

// ApplyLocalRoute installs (nil removes) the local route on the running client
// and probes it once so the UI sees the new state straight away. The stored
// account record is the caller's business (the GUI writes it before calling).
func (e *Engine) ApplyLocalRoute(ctx context.Context, lr *account.LocalRoute) error {
	if lr == nil {
		return e.client.SetLocalRoute("", "", "")
	}
	if err := e.client.SetLocalRoute(lr.Address, lr.Pin, lr.RootID); err != nil {
		return err
	}
	if err := e.client.ProbeLocal(ctx); err != nil {
		_, reason := e.client.Route()
		slog.Info(routeSwitchMessage(transport.RoutePublic, reason, lr.Address), "err", err)
	} else {
		slog.Info(routeSwitchMessage(transport.RouteLocal, "", e.client.LocalAddress()))
	}
	return nil
}
