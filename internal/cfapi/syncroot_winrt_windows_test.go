//go:build windows

package cfapi

import (
	"os"
	"testing"

	"golang.org/x/sys/windows/registry"
)

// dumpKey logs a registry key's values and, recursively, its subkeys.
func dumpKey(t *testing.T, hive registry.Key, path, indent string) {
	t.Helper()
	k, err := registry.OpenKey(hive, path, registry.READ)
	if err != nil {
		return
	}
	defer k.Close()
	names, _ := k.ReadValueNames(-1)
	for _, n := range names {
		if s, _, err := k.GetStringValue(n); err == nil {
			t.Logf("%s%s = %q", indent, n, s)
		} else if d, _, err := k.GetIntegerValue(n); err == nil {
			t.Logf("%s%s = %d (0x%x)", indent, n, d, d)
		} else {
			t.Logf("%s%s = <binary>", indent, n)
		}
	}
	subs, _ := k.ReadSubKeyNames(-1)
	for _, s := range subs {
		t.Logf("%s[%s]", indent, s)
		dumpKey(t, hive, path+`\`+s, indent+"    ")
	}
}

// TestWinRTShellRegistration exercises the real brokered API against the real
// machine registry, so it is opt-in: set NIMBO_WINRT_SYNCROOT_TEST=1 to run it.
//
//	go test ./internal/cfapi -run TestWinRTShellRegistration -v
//
// It registers a throwaway folder, asserts the entry appears under HKLM (the
// hive Explorer actually reads, and the whole point of using the brokered API),
// then unregisters and asserts it is gone.
func TestWinRTShellRegistration(t *testing.T) {
	if os.Getenv("NIMBO_WINRT_SYNCROOT_TEST") != "1" {
		t.Skip("set NIMBO_WINRT_SYNCROOT_TEST=1 to run the live registration test")
	}

	dir := t.TempDir()
	id, err := shellRootID(dir)
	if err != nil {
		t.Fatalf("shellRootID: %v", err)
	}
	t.Logf("registering id=%q path=%q", id, dir)

	if err := registerShellSyncRootWinRT(id, dir, "Nimbo Test", `%SystemRoot%\system32\imageres.dll,-1043`, "1.0", ShellPolicyStatusOnly); err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() { _ = unregisterShellSyncRootWinRT(id) })

	for _, root := range []struct {
		name string
		key  registry.Key
	}{{"HKLM", registry.LOCAL_MACHINE}, {"HKCU", registry.CURRENT_USER}} {
		k, err := registry.OpenKey(root.key, syncRootManager+`\`+id, registry.READ)
		if err != nil {
			t.Logf("%s: not present (%v)", root.name, err)
			continue
		}
		t.Logf("%s: present", root.name)
		dumpKey(t, root.key, syncRootManager+`\`+id, "    ")
		k.Close()
	}

	if _, err := registry.OpenKey(registry.LOCAL_MACHINE, syncRootManager+`\`+id, registry.READ); err != nil {
		t.Fatalf("registration did not reach HKLM, which is the hive Explorer reads: %v", err)
	}

	// Explorer resolves the root's location through UserSyncRoots; without it the
	// entry describes a provider with no folder attached.
	uk, err := registry.OpenKey(registry.LOCAL_MACHINE, syncRootManager+`\`+id+`\UserSyncRoots`, registry.READ)
	if err != nil {
		t.Fatalf("UserSyncRoots missing: %v", err)
	}
	uk.Close()

	if err := unregisterShellSyncRootWinRT(id); err != nil {
		t.Fatalf("unregister: %v", err)
	}
	if _, err := registry.OpenKey(registry.LOCAL_MACHINE, syncRootManager+`\`+id, registry.READ); err == nil {
		t.Fatal("entry still present in HKLM after Unregister")
	}
}
