package transport

import (
	"context"
	"errors"
	"fmt"
)

// StatusError is an unexpected HTTP response from the server. Its message
// keeps the exact historical "…server returned <status>…" wording — several
// callers (and log greps) key on that phrase — while letting error-handling
// code branch on the status code instead of parsing strings.
type StatusError struct {
	Op      string // e.g. "PUT chunk 00045"
	Path    string
	Code    int    // e.g. 507
	Status  string // e.g. "507 Insufficient Storage"
	Snippet string // trimmed slice of the response body
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s %q: server returned %s: %s", e.Op, e.Path, e.Status, e.Snippet)
}

// StatusCode returns the HTTP status code carried by err (unwrapping as
// needed), or 0 when err holds none.
func StatusCode(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code
	}
	return 0
}

// ErrNotFound marks a listing of a path the server does not have (PROPFIND
// 404). It is final, not transient: Retryable says no to it.
var ErrNotFound = errors.New("not found on the server")

// Retryable reports whether err is worth another attempt: transient server
// distress (5xx, 429) and network-level failures, but not deliberate refusals
// (other 4xx — bad request, forbidden, locked, quota) or the caller giving up
// (context cancellation).
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrNotFound) {
		return false
	}
	if code := StatusCode(err); code != 0 {
		// 501 (verb unsupported) and 507 (quota full) are refusals that won't
		// heal on a retry loop's timescale, unlike a 500/502/503/504 blip.
		return (code >= 500 && code != 501 && code != 507) || code == 429
	}
	return true // no HTTP status — a network/transport failure
}
