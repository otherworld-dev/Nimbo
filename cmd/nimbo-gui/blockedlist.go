package main

import (
	"path/filepath"

	"github.com/otherworld/nimbo/internal/agent"
)

// blockedItem is the settings row for one entry the server's naming rules
// stop from syncing. A folder is never offered escaping: directories are never
// escaped (engine.FilterBlocked blocks a forbidden folder name outright, in
// both sync modes), so opting its extension in would change nothing and the
// row would come straight back.
func blockedItem(b agent.BlockedFile, canEscape func(base string) bool) BlockedItem {
	base := filepath.Base(b.Path)
	return BlockedItem{
		Abs: b.Abs, Path: b.Path, Reason: b.Reason,
		Ext:       filepath.Ext(base),
		Escapable: !b.IsDir && canEscape(base),
	}
}
