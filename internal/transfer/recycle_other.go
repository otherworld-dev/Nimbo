//go:build !windows

package transfer

import "errors"

// recycle is Windows-only; elsewhere the caller falls back to a plain delete.
func recycle(string) error { return errors.New("no recycle bin on this platform") }
