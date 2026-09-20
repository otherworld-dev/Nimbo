//go:build !windows

package atomicfile

import "syscall"

func crossVolumeErr() error { return syscall.EXDEV }
