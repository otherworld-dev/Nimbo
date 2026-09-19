//go:build !windows

package transfer

// writerPresent can't tell elsewhere: other platforms have no sharing modes to
// ask, so a file caught changing is simply tried again.
func writerPresent(string) error { return nil }
