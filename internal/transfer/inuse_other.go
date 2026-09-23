//go:build !windows

package transfer

import "os"

// writerPresent can't tell elsewhere: other platforms have no sharing modes to
// ask, so a file caught changing is simply tried again.
func writerPresent(string) error { return nil }

// lockedOut: other platforms don't refuse a read because another program has
// the file open.
func lockedOut(error) bool { return false }

// openShared is os.Open: other platforms don't refuse a rename over an open file.
func openShared(path string) (*os.File, error) { return os.Open(path) }
