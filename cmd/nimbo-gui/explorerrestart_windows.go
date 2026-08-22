//go:build windows

package main

import (
	"os/exec"
	"strings"
	"time"
)

// restartExplorer restarts every File Explorer process. Explorer attaches the
// cloud Status column (and releases it) only at process start — verified live
// 2026-08-22: after a sync root registers, a fresh WINDOW in the old Explorer
// process never gains the column, while a fresh PROCESS shows it fully
// populated. Open folder windows close; that's why the toast asks first.
func restartExplorer() {
	kill := exec.Command("taskkill", "/f", "/im", "explorer.exe")
	hideConsole(kill)
	_ = kill.Run()
	// Windows normally relaunches the shell by itself; only start one if it
	// didn't — starting a second shell just opens a stray folder window.
	for i := 0; i < 20; i++ {
		time.Sleep(250 * time.Millisecond)
		if explorerRunning() {
			return
		}
	}
	_ = exec.Command("explorer.exe").Start()
}

func explorerRunning() bool {
	c := exec.Command("tasklist", "/fi", "imagename eq explorer.exe", "/fo", "csv", "/nh")
	hideConsole(c)
	out, err := c.Output()
	return err == nil && strings.Contains(strings.ToLower(string(out)), "explorer.exe")
}
