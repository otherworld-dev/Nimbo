package main

import "testing"

// The tray icon and the flyout dot infer "the engine is working" from the status
// TEXT. The scan now reports itself in stages ("Checking server…", "Comparing
// changes…"), none of which contain "sync" or "scan" — without widening the
// match, the tray would stop spinning and the dot would go grey mid-scan.
func TestIsBusyStatusCoversEveryScanPhase(t *testing.T) {
	busy := []string{
		// scan stages emitted by agent.computePlan
		"Reading your file list…",
		"Checking server… 1,204 folders",
		"Checking your files… 34,120",
		"Comparing changes…",
		"Matching moved files… 12",
		// pre-existing busy states
		"Scanning…",
		"Syncing…",
		"Moving your folder…",
	}
	for _, s := range busy {
		if !isBusyStatus(s) {
			t.Errorf("isBusyStatus(%q) = false, want true", s)
		}
	}
}

func TestIsBusyStatusRejectsRestingStates(t *testing.T) {
	idle := []string{
		"Up to date",
		"Paused",
		"Offline",
		"Error",
		"Sign in again",
		"Starting…",
		"",
	}
	for _, s := range idle {
		if isBusyStatus(s) {
			t.Errorf("isBusyStatus(%q) = true, want false", s)
		}
	}
}

// Pre-existing quirk, preserved deliberately: a halted sync still reads as busy
// because the message contains "sync". Changing it is a tray-behaviour change
// outside this work — recorded here so the next person sees it is known, not an
// oversight.
func TestIsBusyStatusKnownQuirkHaltedSyncReadsBusy(t *testing.T) {
	if !isBusyStatus("Sync stopped — local folder missing") {
		t.Skip("halted-sync quirk has been fixed; update this test and trayState")
	}
}
