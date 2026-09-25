//go:build !windows

package main

// Explorer corner badges are a Windows shell feature; elsewhere they are never
// registered and there is nothing to enable or offer.
func badgesRegistered() bool { return false }

func (a *App) enableBadges() string { return "sync badges are only available on Windows" }

func (a *App) maybeOfferBadges() {}
