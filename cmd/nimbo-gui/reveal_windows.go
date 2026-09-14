//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// explorerSelectCmdLine is the exact command line that makes Explorer open an
// item's folder with the item selected.
//
// Verified live 2026-09-14: Explorer parses `/select,"<path>"` but NOT
// `"/select,<path>"` — and the latter is what Go's default argument quoting
// produces for any path containing a space, so exec.Command("explorer",
// "/select,"+path) silently opened the Documents library instead of the file
// for every user whose sync folder has a space in it. The line is therefore
// built by hand and handed to CreateProcess verbatim.
func explorerSelectCmdLine(path string) string {
	return `explorer.exe /select,"` + path + `"`
}

// revealPath opens Explorer with the given file or folder selected.
func revealPath(path string) {
	cmd := exec.Command("explorer.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: explorerSelectCmdLine(path)}
	_ = cmd.Start()
}
