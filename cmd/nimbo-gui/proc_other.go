//go:build !windows

package main

import "os/exec"

// hideConsole is a no-op off Windows (no console-window concept to suppress).
func hideConsole(*exec.Cmd) {}
