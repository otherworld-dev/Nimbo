package main

import (
	"path/filepath"
	"testing"

	"github.com/otherworld/nimbo/internal/config"
)

// The revert dialog's "won't download" count must use the rules live sync
// will actually apply once the switch restores the remembered folders (#512).
func TestRevertSkip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Nimbo")
	docs := filepath.Join(dir, "Documents")
	skip := revertSkip(dir, []string{"node_modules"}, []config.SyncPair{
		{LocalDir: docs, RemoteRoot: "/Documents", Excludes: []string{"Archive"}},
		{LocalDir: filepath.Join(t.TempDir(), "elsewhere"), RemoteRoot: "/Other", Excludes: []string{"Photos"}},
	})
	cases := map[string]bool{
		"node_modules/pkg/index.js":       true,  // global rule, outside any folder
		"Documents/app/node_modules/a.js": true,  // global rule, inside a folder
		"Documents/Archive/2019.pdf":      true,  // that folder's own exclude
		"Archive/2019.pdf":                false, // the exclude is folder-relative
		"Documents/Reports/q1.pdf":        false,
		"Photos/cat.jpg":                  false, // another folder's exclude, not under dir
		"src/node_modules.txt":            false,
	}
	for rel, want := range cases {
		if got := skip(rel); got != want {
			t.Errorf("skip(%q) = %v, want %v", rel, got, want)
		}
	}
}
