//go:build windows

package shellmenu

import (
	"fmt"
	"os"
	"sort"
	"testing"

	"golang.org/x/sys/windows/registry"
)

// scratchRoots points the menu at a throwaway key under HKCU for one test, so
// Register writes there and the real Explorer menu is left alone.
func scratchRoots(t *testing.T) {
	t.Helper()
	base := fmt.Sprintf(`Software\NimboShellMenuTest-%d`, os.Getpid())
	of, od, olf, old := fileRoot, dirRoot, legacyFile, legacyDir
	fileRoot = base + `\Classes\*\shell\Nimbo`
	dirRoot = base + `\Classes\Directory\shell\Nimbo`
	legacyFile = base + `\Classes\*\shell\NextClientShare`
	legacyDir = base + `\Classes\Directory\shell\NextClientShare`
	t.Cleanup(func() {
		deleteTree(base)
		fileRoot, dirRoot, legacyFile, legacyDir = of, od, olf, old
	})
}

// child is one entry of the "Nimbo" submenu as the registry holds it.
type child struct{ id, label, command string }

func readChildren(t *testing.T, root string) []child {
	t.Helper()
	k, err := registry.OpenKey(registry.CURRENT_USER, root+`\shell`, registry.READ)
	if err != nil {
		t.Fatalf("open %s\\shell: %v", root, err)
	}
	ids, err := k.ReadSubKeyNames(-1)
	_ = k.Close()
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(ids) // Explorer lists the entries in key order
	var out []child
	for _, id := range ids {
		c := child{id: id}
		for path, into := range map[string]*string{root + `\shell\` + id: &c.label, root + `\shell\` + id + `\command`: &c.command} {
			ck, err := registry.OpenKey(registry.CURRENT_USER, path, registry.QUERY_VALUE)
			if err != nil {
				t.Fatalf("open %s: %v", path, err)
			}
			*into, _, err = ck.GetStringValue("")
			_ = ck.Close()
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
		}
		out = append(out, c)
	}
	return out
}

func TestRegisterWritesTheMenuForFilesAndFolders(t *testing.T) {
	scratchRoots(t)
	const exe = `C:\Program Files\Nimbo\nimbo-gui.exe`
	if err := Register(exe); err != nil {
		t.Fatal(err)
	}
	if !Enabled() {
		t.Fatal("Enabled is false after Register")
	}
	cmd := func(arg string) string { return `"` + exe + `" ` + arg + ` "%1"` }
	wantFiles := []child{
		{"01share", "Share", cmd("--share")},
		{"02versions", "Version history", cmd("--versions")},
		{"03keep", "Always keep on this device", cmd("--keep")},
		{"04free", "Free up space", cmd("--free")},
		{"05syncfirst", "Sync this first", cmd("--sync-first")},
	}
	wantDirs := []child{ // no version history for a folder
		{"01share", "Share", cmd("--share")},
		{"03keep", "Always keep on this device", cmd("--keep")},
		{"04free", "Free up space", cmd("--free")},
		{"05syncfirst", "Sync this first", cmd("--sync-first")},
	}
	if got := readChildren(t, fileRoot); fmt.Sprint(got) != fmt.Sprint(wantFiles) {
		t.Errorf("files menu:\n got %v\nwant %v", got, wantFiles)
	}
	if got := readChildren(t, dirRoot); fmt.Sprint(got) != fmt.Sprint(wantDirs) {
		t.Errorf("folders menu:\n got %v\nwant %v", got, wantDirs)
	}

	// The parent is a cascade titled "Nimbo" with the app's icon.
	for _, root := range []string{fileRoot, dirRoot} {
		k, err := registry.OpenKey(registry.CURRENT_USER, root, registry.QUERY_VALUE)
		if err != nil {
			t.Fatal(err)
		}
		title, _, _ := k.GetStringValue("MUIVerb")
		icon, _, _ := k.GetStringValue("Icon")
		_, _, subErr := k.GetStringValue("SubCommands")
		_ = k.Close()
		if title != "Nimbo" || icon != exe+",0" || subErr != nil {
			t.Errorf("%s: title %q, icon %q, SubCommands err %v", root, title, icon, subErr)
		}
	}

	if err := Unregister(); err != nil {
		t.Fatal(err)
	}
	if Enabled() {
		t.Fatal("Enabled is true after Unregister")
	}
}

// Registering again replaces the menu rather than adding to it: an entry an
// older version wrote, and no longer does, goes.
func TestRegisterReplacesAnOlderMenu(t *testing.T) {
	scratchRoots(t)
	if err := writeChild(fileRoot, "09old", "Something retired", `C:\old.exe`, "--old"); err != nil {
		t.Fatal(err)
	}
	if err := Register(`C:\new.exe`); err != nil {
		t.Fatal(err)
	}
	for _, c := range readChildren(t, fileRoot) {
		if c.id == "09old" {
			t.Fatal("an entry from the older menu is still there")
		}
	}
}
