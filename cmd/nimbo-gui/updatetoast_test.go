package main

import (
	"os"
	"strings"
	"testing"
)

// The "update available" toast's body click must open the SETTINGS window (its
// General tab, where "Check for updates / Update now" live) — not the
// sync-status window. The bug: dispatchToastActivation's "settings" case called
// openStatus("settings"), opening the status window and asking for a tab that
// doesn't exist there, so the click landed on the wrong menu entirely.
func TestUpdateToastRoutesToSettingsWindow(t *testing.T) {
	src, err := os.ReadFile("service.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)

	// The update toast still carries action=settings on its body.
	if !strings.Contains(body, `"action=settings"`) {
		t.Error("update toast body action=settings is gone — check updateCheckLoop")
	}

	// Isolate the dispatch function and its settings case.
	start := strings.Index(body, "func (a *App) dispatchToastActivation")
	if start < 0 {
		t.Fatal("dispatchToastActivation not found")
	}
	fn := body[start:]
	if e := strings.Index(fn, "\n}\n"); e >= 0 {
		fn = fn[:e]
	}
	ci := strings.Index(fn, `case "settings":`)
	if ci < 0 {
		t.Fatal(`no case "settings" in dispatchToastActivation`)
	}
	// Take the settings case up to the next case/default.
	caseBody := fn[ci:]
	if n := strings.Index(caseBody[len(`case "settings":`):], "\n\tcase "); n >= 0 {
		caseBody = caseBody[:len(`case "settings":`)+n]
	}
	if strings.Contains(caseBody, "openStatus(") {
		t.Error(`update toast still opens the STATUS window (openStatus) from case "settings" — wrong menu`)
	}
	if !strings.Contains(caseBody, "openSettingsTab(") {
		t.Error(`case "settings" should open the Settings window via openSettingsTab`)
	}
	if !strings.Contains(caseBody, `"general"`) {
		t.Error(`update toast should land on the General tab (where "Update now" lives)`)
	}
}
