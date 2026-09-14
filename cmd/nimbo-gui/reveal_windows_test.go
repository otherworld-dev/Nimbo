//go:build windows

package main

import (
	"os"
	"testing"
)

// Explorer only understands /select when the path alone is quoted. Go's own
// quoting wraps the whole argument once it contains a space, which Explorer
// answers by opening Documents — so the command line is hand-built, and this
// pins its shape.
func TestExplorerSelectCmdLineQuotesOnlyThePath(t *testing.T) {
	got := explorerSelectCmdLine(`C:\Users\Adam\My Sync\a file.txt`)
	want := `explorer.exe /select,"C:\Users\Adam\My Sync\a file.txt"`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// Live check, opt-in: actually launches Explorer on the path in
// NIMBO_LIVE_REVEAL so an operator can confirm the selection by eye (or via
// the Shell.Application COM windows list). Skipped in normal runs.
func TestLiveReveal(t *testing.T) {
	p := os.Getenv("NIMBO_LIVE_REVEAL")
	if p == "" {
		t.Skip("set NIMBO_LIVE_REVEAL=<path> to launch Explorer for real")
	}
	revealPath(p)
}
