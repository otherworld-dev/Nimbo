//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// hideConsole makes a child console process (e.g. the PowerShell folder-picker
// fallback) run without flashing a console window. CREATE_NO_WINDOW suppresses
// the console only — any GUI dialog the process opens still appears.
func hideConsole(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}
