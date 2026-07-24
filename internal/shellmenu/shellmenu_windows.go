//go:build windows

// Package shellmenu manages the Windows Explorer right-click "Nimbo" cascading
// menu. It registers (under HKCU, no admin) a parent "Nimbo" submenu whose
// children launch the app: "Share" (--share), "Version history" (--versions,
// files only), and the on-demand actions "Always keep on this device" (--keep)
// and "Free up space" (--free). Cascading menus are built with the documented
// SubCommands + nested `shell` subkey technique (no COM).
package shellmenu

import "golang.org/x/sys/windows/registry"

const (
	fileRoot = `Software\Classes\*\shell\Nimbo`
	dirRoot  = `Software\Classes\Directory\shell\Nimbo`
	// legacy single verb from earlier versions, removed on (re)register.
	legacyFile = `Software\Classes\*\shell\NextClientShare`
	legacyDir  = `Software\Classes\Directory\shell\NextClientShare`
)

// Supported reports whether the Explorer integration can be configured here.
func Supported() bool { return true }

// Enabled reports whether the Nimbo context menu is registered.
func Enabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, fileRoot, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	_ = k.Close()
	return true
}

// Register installs the cascading "Nimbo" menu, pointing at exePath. Idempotent.
func Register(exePath string) error {
	// Remove any prior layout so children/labels don't go stale.
	_ = Unregister()

	// Files: Nimbo ▸ Share / Version history.
	if err := writeParent(fileRoot, exePath); err != nil {
		return err
	}
	if err := writeChild(fileRoot, "01share", "Share", exePath, "--share"); err != nil {
		return err
	}
	if err := writeChild(fileRoot, "02versions", "Version history", exePath, "--versions"); err != nil {
		return err
	}
	if err := writeChild(fileRoot, "03keep", "Always keep on this device", exePath, "--keep"); err != nil {
		return err
	}
	if err := writeChild(fileRoot, "04free", "Free up space", exePath, "--free"); err != nil {
		return err
	}
	// Folders: Nimbo ▸ Share / keep / free (no versions for folders).
	if err := writeParent(dirRoot, exePath); err != nil {
		return err
	}
	if err := writeChild(dirRoot, "01share", "Share", exePath, "--share"); err != nil {
		return err
	}
	if err := writeChild(dirRoot, "03keep", "Always keep on this device", exePath, "--keep"); err != nil {
		return err
	}
	if err := writeChild(dirRoot, "04free", "Free up space", exePath, "--free"); err != nil {
		return err
	}
	return nil
}

// writeParent creates a cascading-menu parent ("Nimbo") at root.
func writeParent(root, exePath string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, root, registry.WRITE)
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.SetStringValue("MUIVerb", "Nimbo"); err != nil {
		return err
	}
	_ = k.SetStringValue("Icon", exePath+",0")
	// Empty SubCommands + a nested `shell` subkey ⇒ Explorer builds a cascade.
	return k.SetStringValue("SubCommands", "")
}

// writeChild adds a child verb under root\shell\<id> that runs exePath with arg.
func writeChild(root, id, label, exePath, arg string) error {
	base := root + `\shell\` + id
	k, _, err := registry.CreateKey(registry.CURRENT_USER, base, registry.WRITE)
	if err != nil {
		return err
	}
	if err := k.SetStringValue("", label); err != nil {
		_ = k.Close()
		return err
	}
	_ = k.Close()
	ck, _, err := registry.CreateKey(registry.CURRENT_USER, base+`\command`, registry.WRITE)
	if err != nil {
		return err
	}
	defer ck.Close()
	return ck.SetStringValue("", `"`+exePath+`" `+arg+` "%1"`)
}

// Unregister removes the Nimbo menu (and any legacy single verb).
func Unregister() error {
	for _, root := range []string{fileRoot, dirRoot, legacyFile, legacyDir} {
		deleteTree(root)
	}
	return nil
}

// deleteTree removes a registry key and all of its subkeys.
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
