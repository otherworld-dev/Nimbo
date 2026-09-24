//go:build windows

package cfapi

import (
	"os/user"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// NimboShellSyncRoots lists this user's sync roots registered by any Nimbo
// build (id "Nimbo!<SID>!…"), read from the HKLM SyncRootManager keys that
// Explorer itself reads. Registry reads are not redirected by the MSIX
// container, so a packaged build sees the real registrations.
func NimboShellSyncRoots() []ShellSyncRoot {
	u, err := user.Current()
	if err != nil {
		return nil
	}
	prefix := "Nimbo!" + u.Uid + "!"
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, syncRootManager, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return nil
	}
	names, _ := k.ReadSubKeyNames(-1)
	_ = k.Close()
	var out []ShellSyncRoot
	for _, id := range names {
		if !strings.HasPrefix(id, prefix) {
			continue
		}
		r := ShellSyncRoot{ID: id}
		if rk, err := registry.OpenKey(registry.LOCAL_MACHINE, syncRootManager+`\`+id, registry.QUERY_VALUE); err == nil {
			r.Icon, _, _ = rk.GetStringValue("IconResource")
			r.NamespaceCLSID, _, _ = rk.GetStringValue("NamespaceCLSID")
			_ = rk.Close()
		}
		if uk, err := registry.OpenKey(registry.LOCAL_MACHINE, syncRootManager+`\`+id+`\UserSyncRoots`, registry.QUERY_VALUE); err == nil {
			r.Path, _, _ = uk.GetStringValue(u.Uid)
			_ = uk.Close()
		}
		out = append(out, r)
	}
	return out
}

// UnregisterShellSyncRootByID removes a sync root by its registration id
// rather than by the id its path would hash to today, which an older build
// may not have used, and drops the filter registration for its folder.
// Unregistering strips the cloud state from the folder: only for roots no
// account uses.
func UnregisterShellSyncRootByID(id, path string) {
	_ = unregisterShellSyncRootWinRT(id)
	removeLegacyHKCUEntry(id)
	if path != "" {
		_ = UnregisterSyncRoot(path)
	}
}
