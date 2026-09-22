package main

import (
	"testing"

	"github.com/otherworld/nimbo/internal/agent"
)

// Directories are never escaped (engine.FilterBlocked blocks a forbidden
// folder name outright), so the "sync it anyway" offer that opts an extension
// into escaping must not appear on a blocked folder: taking it would enable
// escaping and leave the folder exactly as blocked (Deck #554, item 3).
func TestBlockedItemNeverOffersEscapingForADirectory(t *testing.T) {
	always := func(string) bool { return true }
	if it := blockedItem(agent.BlockedFile{Path: "web/.htaccess", IsDir: true}, always); it.Escapable {
		t.Fatal("a blocked folder was offered escaping")
	}
	if it := blockedItem(agent.BlockedFile{Path: "web/.htaccess"}, always); !it.Escapable || it.Ext != ".htaccess" {
		t.Fatalf("a blocked file lost its offer: %+v", it)
	}
}
