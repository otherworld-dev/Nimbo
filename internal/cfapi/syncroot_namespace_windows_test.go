//go:build windows

package cfapi

import (
	"fmt"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"
)

// Windows records the navigation-pane node it creates for a cloud sync root
// as NamespaceCLSID on the root's SyncRootManager key. The reader is what the
// sidebar toggle relies on to find that node, so pin how it reads the value
// and what it answers when there is nothing to read. The test uses a private
// key under HKCU rather than the real SyncRootManager path.
func TestNamespaceCLSIDAt(t *testing.T) {
	base := fmt.Sprintf(`Software\Nimbo\cfapi-test\%d-%d`, os.Getpid(), time.Now().UnixNano())
	k, _, err := registry.CreateKey(registry.CURRENT_USER, base, registry.SET_VALUE)
	if err != nil {
		t.Fatalf("create test key: %v", err)
	}
	t.Cleanup(func() {
		_ = registry.DeleteKey(registry.CURRENT_USER, base)
		_ = registry.DeleteKey(registry.CURRENT_USER, `Software\Nimbo\cfapi-test`)
	})
	const clsid = "{FAE6C166-B8D9-4EEB-912D-7D7609AE123F}"
	if err := k.SetStringValue("NamespaceCLSID", " "+clsid+" "); err != nil {
		t.Fatalf("set value: %v", err)
	}
	_ = k.Close()

	if got := namespaceCLSIDAt(registry.CURRENT_USER, base); got != clsid {
		t.Errorf("namespaceCLSIDAt = %q, want %q (trimmed)", got, clsid)
	}
	if got := namespaceCLSIDAt(registry.CURRENT_USER, base+`\missing`); got != "" {
		t.Errorf("missing key: got %q, want empty", got)
	}
	// A key without the value is a root Windows has not given a node.
	k2, _, err := registry.CreateKey(registry.CURRENT_USER, base+`\novalue`, registry.SET_VALUE)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_ = k2.Close()
	if got := namespaceCLSIDAt(registry.CURRENT_USER, base+`\novalue`); got != "" {
		t.Errorf("no value: got %q, want empty", got)
	}
}

// A path that was never registered has no node.
func TestShellSyncRootNamespaceCLSIDUnknownPath(t *testing.T) {
	if got := ShellSyncRootNamespaceCLSID(`C:\definitely\not\a\sync\root\` + fmt.Sprint(time.Now().UnixNano())); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}
