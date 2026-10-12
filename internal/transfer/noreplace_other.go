//go:build !windows && !linux

package transfer

// renameNoReplace renames from to to, failing with an error matching
// os.ErrExist when something is already at to.
func renameNoReplace(from, to string) error { return renameNoReplaceFallback(from, to) }
