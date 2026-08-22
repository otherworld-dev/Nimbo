package main

import (
	"strings"
	"testing"
)

// TestConflictNameDoesNotStackMarkers pins the on-demand write-back's conflict
// naming to the same rule as the live engine's: a conflict of an
// already-conflicted copy replaces the old marker instead of appending another.
// Stacking is how a test-VM file grew six markers and crossed MAX_PATH, at
// which point Explorer and Notepad could no longer open it.
func TestConflictNameDoesNotStackMarkers(t *testing.T) {
	got := conflictName("Notes/New Text Document (conflicted copy 2026-08-16 000722).txt")
	if strings.Count(got, "(conflicted copy") != 1 {
		t.Fatalf("marker stacked: %q", got)
	}
	if !strings.HasPrefix(got, "Notes/New Text Document (conflicted copy ") || !strings.HasSuffix(got, ").txt") {
		t.Fatalf("unexpected shape: %q", got)
	}

	// Six stacked markers (the real VM filename) collapse back to one.
	stacked := "New Text Document" + strings.Repeat(" (conflicted copy 2026-08-16 000722)", 6) + ".txt"
	if got := conflictName(stacked); strings.Count(got, "(conflicted copy") != 1 {
		t.Fatalf("stacked markers survived: %q", got)
	}

	// A dotfile keeps its whole name (no extension split) and still gets
	// exactly one marker.
	got = conflictName(".htaccess (conflicted copy 2025-01-02 030405)")
	if !strings.HasPrefix(got, ".htaccess (conflicted copy ") || strings.Count(got, "(conflicted copy") != 1 {
		t.Fatalf("dotfile: got %q", got)
	}
}
