package main

import (
	"os"
	"strings"
	"testing"
)

// Explorer attaches the cloud Status column (and drops it again) only at
// process start — verified live 2026-08-22: after registering a sync root, a
// fresh WINDOW in the old Explorer process never gains the column, while a
// fresh PROCESS shows it fully populated. So every file-availability switch
// that registers or unregisters a sync root must offer an Explorer restart,
// or the user sits in front of stale columns wondering where the icons went.
//
// Source-inspection test (the SetSyncMode flow is glue no unit harness can
// run): the switch paths must gate the offer on the PREVIOUS mode so a
// first-run "live" setup — where no root ever existed — doesn't toast noise.
func TestModeSwitchOffersExplorerRestart(t *testing.T) {
	src, err := os.ReadFile("service.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func (a *App) SetSyncMode(mode string) string {")
	if start < 0 {
		t.Fatal("SetSyncMode not found")
	}
	end := strings.Index(body[start:], "\n}")
	if end < 0 {
		t.Fatal("SetSyncMode end not found")
	}
	fn := body[start : start+end]

	if !strings.Contains(fn, "offerExplorerRestart") {
		t.Error("SetSyncMode never offers an Explorer restart — mode switches leave stale Status columns/icons")
	}
	if !strings.Contains(fn, "prevMode") {
		t.Error("the offer isn't gated on the previous mode — a first-run live setup would toast for no reason")
	}

	if !strings.Contains(body, `case "explorer-restart":`) {
		t.Error("dispatchToastActivation has no explorer-restart case — the toast would be a dead click")
	}
}
