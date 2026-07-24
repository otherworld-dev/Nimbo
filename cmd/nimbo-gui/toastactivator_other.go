//go:build !windows

package main

// toastActivationHandler / registerToastActivator are Windows-only no-ops here.
var toastActivationHandler func(args string)

func registerToastActivator() {}
