//go:build !windows

package main

import (
	"os/exec"
	"path/filepath"
	"runtime"
)

// revealPath opens the file manager with the given item selected, falling back
// to opening its folder where selection isn't supported.
func revealPath(path string) {
	if runtime.GOOS == "darwin" {
		_ = exec.Command("open", "-R", path).Start()
		return
	}
	openPath(filepath.Dir(path))
}
