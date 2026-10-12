package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/otherworld/nimbo/internal/config"
	"github.com/otherworld/nimbo/internal/vfs"
)

func fateName(f vfs.StubFate) string {
	switch f {
	case vfs.StubDownloads:
		return "downloads"
	case vfs.StubIgnored:
		return "ignored"
	case vfs.StubUnsynced:
		return "unsynced"
	}
	return "unknown"
}

func checkFates(t *testing.T, fate func(string) vfs.StubFate, cases map[string]vfs.StubFate) {
	t.Helper()
	for rel, want := range cases {
		if got := fate(rel); got != want {
			t.Errorf("fate(%q) = %s, want %s", rel, fateName(got), fateName(want))
		}
	}
}

// The revert dialog's counts must use what live sync will actually do once the
// switch restores the remembered folders: a stub inside one downloads unless
// the rules skip it (#512), and a stub outside all of them is not synced at
// all, whatever the rules say (#948).
func TestRevertFate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Nimbo")
	docs := filepath.Join(dir, "Documents")
	fate := revertFate(dir, []string{"node_modules"}, []config.SyncPair{
		{LocalDir: docs, RemoteRoot: "/Documents", Excludes: []string{"Archive"}},
		{LocalDir: filepath.Join(t.TempDir(), "elsewhere"), RemoteRoot: "/Other", Excludes: []string{"Photos"}},
	})
	checkFates(t, fate, map[string]vfs.StubFate{
		"Documents/Reports/q1.pdf":        vfs.StubDownloads,
		"Documents/app/node_modules/a.js": vfs.StubIgnored,   // global rule, inside a folder
		"Documents/Archive/2019.pdf":      vfs.StubIgnored,   // that folder's own exclude
		"Documents/src/node_modules.txt":  vfs.StubDownloads, // name only looks like the rule
		"Archive/2019.pdf":                vfs.StubUnsynced,  // the exclude is folder-relative; no folder here
		"node_modules/pkg/index.js":       vfs.StubUnsynced,  // no folder covers it: that outranks the rule
		"Photos/cat.jpg":                  vfs.StubUnsynced,  // the other folder is not under dir
		"notes.txt":                       vfs.StubUnsynced,  // beside the folder, not in it
		"Documents2/q1.pdf":               vfs.StubUnsynced,  // a name prefix is not a parent
	})
}

// A whole-account folder covers everything under the mount, so only the rules
// decide.
func TestRevertFateWholeAccountFolder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Nimbo")
	fate := revertFate(dir, []string{"node_modules"}, []config.SyncPair{
		{LocalDir: dir, RemoteRoot: "/", Excludes: []string{"Archive"}},
	})
	checkFates(t, fate, map[string]vfs.StubFate{
		"notes.txt":                 vfs.StubDownloads,
		"Documents/Reports/q1.pdf":  vfs.StubDownloads,
		"node_modules/pkg/index.js": vfs.StubIgnored,
		"Archive/2019.pdf":          vfs.StubIgnored,
	})
}

// #948, as seen on the test VM: after a "Start fresh" switch to virtual files
// no folder is remembered, so the switch back restores none and every
// online-only stub is unsynced. None may be promised as a download, and none
// is "ignored by your exclusions" either.
func TestRevertFateNoRememberedFolders(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Nimbo")
	for name, remembered := range map[string][]config.SyncPair{
		"nil":   nil,
		"empty": {},
	} {
		fate := revertFate(dir, []string{"node_modules"}, remembered)
		for _, rel := range []string{"notes.txt", "Documents/Reports/q1.pdf", "node_modules/pkg/index.js"} {
			if got := fate(rel); got != vfs.StubUnsynced {
				t.Errorf("%s: fate(%q) = %s, want unsynced", name, rel, fateName(got))
			}
		}
	}
}

// The completion toast must not say folders were restored when there are none.
func TestRevertDoneToast(t *testing.T) {
	if got := revertDoneToast(1234, 2); !strings.Contains(got, "1,234 converted in place") || !strings.Contains(got, "sync folders restored") {
		t.Errorf("with folders: %q", got)
	}
	got := revertDoneToast(1234, 0)
	if !strings.Contains(got, "1,234 converted in place") || !strings.Contains(got, "No sync folder is set up") {
		t.Errorf("without folders: %q", got)
	}
	if strings.Contains(got, "restored") {
		t.Errorf("without folders the toast must not claim a restore: %q", got)
	}
}
