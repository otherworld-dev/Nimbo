//go:build windows

package cfapi

import (
	"os"
	"testing"

	"golang.org/x/sys/windows/registry"
)

// TestFlagBits decodes the SyncRootManager "Flags" word by registering the same
// folder with one property varied at a time and reading back what Windows wrote.
//
// Flags is undocumented, and Nimbo's registration produces 0x22 where OneDrive's
// is 0x522. Rather than guess at the missing bits, this derives them. Opt in with
// NIMBO_WINRT_SYNCROOT_TEST=1.
func TestFlagBits(t *testing.T) {
	if os.Getenv("NIMBO_WINRT_SYNCROOT_TEST") != "1" {
		t.Skip("set NIMBO_WINRT_SYNCROOT_TEST=1 to run the live registration test")
	}

	type variant struct {
		name string
		pol  ShellPolicy
	}
	for _, v := range []variant{
		{"status-only, no pinning", ShellPolicy{Hydration: 3, Population: 2}},
		{"on-demand, no pinning", ShellPolicy{Hydration: 2, Population: 1}},
		{"on-demand + AllowPinning", ShellPolicy{Hydration: 2, Population: 1, AllowPinning: true}},
		{"on-demand + AllowPinning + Siblings", ShellPolicy{Hydration: 2, Population: 1, AllowPinning: true, ShowSiblings: true}},
		{"on-demand + AllowPinning + InSync writetimes", ShellPolicy{Hydration: 2, Population: 1, InSync: 0x300, AllowPinning: true}},
		{"status-only + AllowPinning", ShellPolicy{Hydration: 3, Population: 2, AllowPinning: true}},
	} {
		t.Run(v.name, func(t *testing.T) {
			dir := t.TempDir()
			id, err := shellRootID(dir)
			if err != nil {
				t.Fatal(err)
			}
			err = registerShellSyncRootWinRT(id, dir, "Nimbo Flags", `%SystemRoot%\system32\imageres.dll,-1043`, "1.0", v.pol)
			if err != nil {
				t.Fatalf("register: %v", err)
			}
			defer unregisterShellSyncRootWinRT(id)

			k, err := registry.OpenKey(registry.LOCAL_MACHINE, syncRootManager+`\`+id, registry.READ)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer k.Close()
			flags, _, err := k.GetIntegerValue("Flags")
			if err != nil {
				t.Fatalf("Flags: %v", err)
			}
			names, _ := k.ReadValueNames(-1)
			t.Logf("Flags = %d (0x%x)   values=%v", flags, flags, names)
		})
	}
	t.Log("OneDrive for comparison: Flags = 1314 (0x522)")
}
