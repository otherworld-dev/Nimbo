//go:build !windows

package transfer

import "os"

// writerPresent can't tell elsewhere: other platforms have no sharing modes to
// ask, so a file caught changing is simply tried again.
func writerPresent(string) error { return nil }

// openShared is os.Open: other platforms don't refuse a rename over an open file.
func openShared(path string) (*os.File, error) { return os.Open(path) }
