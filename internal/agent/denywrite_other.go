//go:build !windows

package agent

import "errors"

// denyWriteHandle is the non-Windows stand-in. Nothing holds anything.
type denyWriteHandle struct{}

func (d *denyWriteHandle) Close() error { return nil }

// holdDenyWrite is unavailable off Windows.
//
// POSIX advisory locks would not do the job: they only bind processes that
// choose to check, whereas the whole point here is to refuse an editor that
// knows nothing about us. The lock warning still works on Linux — the file
// simply is not held, so a second editor is told rather than stopped.
func holdDenyWrite(path string) (*denyWriteHandle, error) {
	return nil, errors.New("deny-write handles are Windows-only")
}

// fileOnlineOnly: there are no on-demand placeholders off Windows.
var fileOnlineOnly = func(path string) bool { return false }
