//go:build windows

package cfapi

import (
	"os"
	"testing"

	"golang.org/x/sys/windows/registry"
)

// TestRegistrationOrder answers the question that decides whether adding the
// brokered shell registration can break on-demand mounting: does
// StorageProviderSyncRootManager.Register cooperate with CfRegisterSyncRoot on
// the same folder, and does the order matter?
//
// Both must succeed, because they do different jobs — CfRegisterSyncRoot binds
// the folder to the cloud-filter driver with our provider GUID and policies,
// while Register supplies the metadata Explorer renders. Opt in with
// NIMBO_WINRT_SYNCROOT_TEST=1.
func TestRegistrationOrder(t *testing.T) {
	if os.Getenv("NIMBO_WINRT_SYNCROOT_TEST") != "1" {
		t.Skip("set NIMBO_WINRT_SYNCROOT_TEST=1 to run the live registration test")
	}

	t.Run("shell first, then filter", func(t *testing.T) {
		dir := t.TempDir()
		id, err := shellRootID(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := registerShellSyncRootWinRT(id, dir, "Nimbo Order A", `%SystemRoot%\system32\imageres.dll,-1043`, "1.0", ShellPolicyOnDemand); err != nil {
			t.Fatalf("shell register: %v", err)
		}
		defer unregisterShellSyncRootWinRT(id)

		if err := registerRoot(dir, cfHydrationPolicyFull, cfPopulationPolicyPartial); err != nil {
			t.Fatalf("CfRegisterSyncRoot after shell register: %v", err)
		}
		defer UnregisterSyncRoot(dir)
		t.Log("both registrations succeeded in this order")
	})

	t.Run("filter first, then shell", func(t *testing.T) {
		dir := t.TempDir()
		id, err := shellRootID(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := registerRoot(dir, cfHydrationPolicyFull, cfPopulationPolicyPartial); err != nil {
			t.Fatalf("CfRegisterSyncRoot: %v", err)
		}
		defer UnregisterSyncRoot(dir)

		if err := registerShellSyncRootWinRT(id, dir, "Nimbo Order B", `%SystemRoot%\system32\imageres.dll,-1043`, "1.0", ShellPolicyOnDemand); err != nil {
			t.Fatalf("shell register after CfRegisterSyncRoot: %v", err)
		}
		defer unregisterShellSyncRootWinRT(id)
		t.Log("both registrations succeeded in this order")
	})

	// Re-registering the same root must be a no-op rather than an error: the mount
	// runs on every app start, and a second run must not fail.
	//
	// It must also not CHANGE the NamespaceCLSID. Windows mints that GUID itself
	// and Explorer builds the navigation-pane node from it, so a new one on every
	// launch would leave Explorer pointing at a namespace that no longer exists —
	// which looks exactly like a sync root that renders once and then degrades to
	// a plain folder.
	t.Run("shell register is idempotent and keeps its NamespaceCLSID", func(t *testing.T) {
		dir := t.TempDir()
		id, err := shellRootID(dir)
		if err != nil {
			t.Fatal(err)
		}
		reg := func() {
			t.Helper()
			if err := registerShellSyncRootWinRT(id, dir, "Nimbo Idem", `%SystemRoot%\system32\imageres.dll,-1043`, "1.0", ShellPolicyStatusOnly); err != nil {
				t.Fatalf("register: %v", err)
			}
		}
		clsid := func() string {
			t.Helper()
			k, err := registry.OpenKey(registry.LOCAL_MACHINE, syncRootManager+`\`+id, registry.READ)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer k.Close()
			s, _, err := k.GetStringValue("NamespaceCLSID")
			if err != nil {
				t.Fatalf("NamespaceCLSID: %v", err)
			}
			return s
		}

		reg()
		defer unregisterShellSyncRootWinRT(id)
		first := clsid()
		reg()
		second := clsid()
		t.Logf("NamespaceCLSID: first=%s second=%s", first, second)
		if first != second {
			t.Errorf("re-registering minted a NEW NamespaceCLSID (%s -> %s); Explorer's cached namespace node is orphaned on every mount", first, second)
		}
	})
}
