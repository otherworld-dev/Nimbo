//go:build !windows

// Package overlay is a no-op outside Windows (shell overlay icons are a Windows
// Explorer feature).
package overlay

import "context"

// Server is an inert placeholder on non-Windows platforms.
type Server struct{}

// Serve does nothing and reports success on non-Windows platforms.
func Serve(context.Context, func(string) string) (*Server, error) { return &Server{}, nil }

// NotifyChange is a no-op on non-Windows platforms.
func NotifyChange(string) {}
