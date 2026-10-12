package transfer

import "os"

// renameNoReplaceFallback is renameNoReplace where the OS offers no single
// call for it: look, then rename. A rename onto the file itself (the same
// file under a name that differs only in capitals) is let through.
func renameNoReplaceFallback(from, to string) error {
	if there, err := os.Lstat(to); err == nil {
		self, serr := os.Lstat(from)
		if serr != nil || !os.SameFile(self, there) {
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: os.ErrExist}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.Rename(from, to)
}
