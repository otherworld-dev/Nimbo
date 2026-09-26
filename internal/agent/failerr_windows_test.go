package agent

import "syscall"

// accessDenied is the error the OS returns when it refuses a path.
var accessDenied error = syscall.Errno(5) // ERROR_ACCESS_DENIED
