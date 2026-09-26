//go:build !windows

package agent

import "syscall"

// accessDenied is the error the OS returns when it refuses a path.
var accessDenied error = syscall.EACCES
