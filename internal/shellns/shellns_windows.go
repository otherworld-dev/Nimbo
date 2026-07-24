//go:build windows

// Package shellns adds (and removes) a Nimbo root in the Windows Explorer
// navigation pane. It uses the documented "delegate folder" namespace pattern —
// a CLSID under HKCU that shell32 hosts and points at a target folder — so it
// needs no COM code and no administrator rights.
package shellns

import (
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// NavGUID identifies the Nimbo navigation-pane node (fixed for the app).
const NavGUID = "{B7A4C3E0-9D2F-4E18-A6C1-2F5E8B0D7A10}"

// delegateCLSID is shell32's generic folder-shortcut implementation.
const delegateCLSID = "{0E5AAE11-A475-4c5b-AB00-C66DE400274E}"

const (
	clsidBase  = `Software\Classes\CLSID\` + NavGUID
	nameSpace  = `Software\Microsoft\Windows\CurrentVersion\Explorer\Desktop\NameSpace\` + NavGUID
	hideDeskKy = `Software\Microsoft\Windows\CurrentVersion\Explorer\HideDesktopIcons\NewStartPanel`
)

// Supported reports whether the sidebar entry can be configured here.
func Supported() bool { return true }

// Enabled reports whether the Nimbo sidebar node is registered.
func Enabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, clsidBase, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	_ = k.Close()
	return true
}

// Register pins a navigation-pane root named name, pointing at targetFolder and
// shown with iconPath. Idempotent; updates target/icon on each call.
func Register(name, targetFolder, iconPath string) error {
	if err := setStr(clsidBase, "", name); err != nil {
		return err
	}
	if err := setDword(clsidBase, "System.IsPinnedToNameSpaceTree", 1); err != nil {
		return err
	}
	if err := setDword(clsidBase, "SortOrderIndex", 0x42); err != nil {
		return err
	}
	if err := setStr(clsidBase+`\DefaultIcon`, "", iconPath+",0"); err != nil {
		return err
	}
	if err := setExpand(clsidBase+`\InProcServer32`, "", `%SystemRoot%\system32\shell32.dll`); err != nil {
		return err
	}
	if err := setStr(clsidBase+`\InProcServer32`, "ThreadingModel", "Both"); err != nil {
		return err
	}
	if err := setStr(clsidBase+`\Instance`, "CLSID", delegateCLSID); err != nil {
		return err
	}
	if err := setDword(clsidBase+`\Instance\InitPropertyBag`, "Attributes", 0x11); err != nil {
		return err
	}
	if err := setExpand(clsidBase+`\Instance\InitPropertyBag`, "TargetFolderPath", targetFolder); err != nil {
		return err
	}
	if err := setDword(clsidBase+`\ShellFolder`, "Attributes", 0xF080004D); err != nil {
		return err
	}
	if err := setDword(clsidBase+`\ShellFolder`, "FolderValueFlags", 0x28); err != nil {
		return err
	}
	// Pin into the navigation-pane tree and hide the matching Desktop icon.
	if err := setStr(nameSpace, "", name); err != nil {
		return err
	}
	_ = setDword(hideDeskKy, NavGUID, 1)
	refresh()
	return nil
}

// Unregister removes the sidebar node.
func Unregister() error {
	deleteTree(clsidBase)
	deleteTree(nameSpace)
	delValue(hideDeskKy, NavGUID)
	refresh()
	return nil
}

// --- registry helpers ---

func setStr(path, name, val string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, path, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(name, val)
}

func setExpand(path, name, val string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, path, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetExpandStringValue(name, val)
}

func setDword(path, name string, val uint32) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, path, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetDWordValue(name, val)
}

func delValue(path, name string) {
	k, err := registry.OpenKey(registry.CURRENT_USER, path, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	_ = k.DeleteValue(name)
}

func deleteTree(path string) {
	if k, err := registry.OpenKey(registry.CURRENT_USER, path, registry.READ); err == nil {
		subs, _ := k.ReadSubKeyNames(-1)
		_ = k.Close()
		for _, s := range subs {
			deleteTree(path + `\` + s)
		}
	}
	_ = registry.DeleteKey(registry.CURRENT_USER, path)
}

var (
	shell32            = windows.NewLazySystemDLL("shell32.dll")
	procSHChangeNotify = shell32.NewProc("SHChangeNotify")
)

// refresh tells Explorer to reload its namespace so the change is visible.
func refresh() {
	const SHCNE_ASSOCCHANGED = 0x08000000
	const SHCNF_IDLIST = 0x0000
	_, _, _ = procSHChangeNotify.Call(uintptr(SHCNE_ASSOCCHANGED), uintptr(SHCNF_IDLIST), 0, 0)
}
