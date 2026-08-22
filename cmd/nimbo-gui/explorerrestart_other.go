//go:build !windows

package main

// restartExplorer is Windows-only (the cloud Status column staleness it fixes
// is an Explorer behaviour); a no-op elsewhere.
func restartExplorer() {}
