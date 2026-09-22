//go:build windows

package shellns

import (
	"strings"
	"testing"
)

const testCloudRoot = "{FAE6C166-B8D9-4EEB-912D-7D7609AE123F}"

// The node Windows creates for a cloud sync root is hidden and shown by the
// pinned-to-tree flag on its CLSID, the same value the entry of our own uses.
// Pin what the out-of-container script writes for each direction.
func TestCloudRootPinScriptSetsTheFlag(t *testing.T) {
	key := psQuote(`HKCU:\Software\Classes\CLSID\` + testCloudRoot)
	for _, c := range []struct {
		on   bool
		want string
	}{{false, "-Value 0"}, {true, "-Value 1"}} {
		s := cloudRootPinScript(testCloudRoot, c.on)
		if !strings.Contains(s, key) {
			t.Errorf("on=%v: script never names the node's key %s:\n%s", c.on, key, s)
		}
		if !strings.Contains(s, "-Name "+psQuote(cloudRootPinnedValue)+" "+c.want+" -PropertyType DWord -Force") {
			t.Errorf("on=%v: flag not written as a DWord %s:\n%s", c.on, c.want, s)
		}
		if !strings.Contains(s, "SHChangeNotify") {
			t.Errorf("on=%v: Explorer is never told to reload", c.on)
		}
	}
}

// Windows creates the node when the sync root is registered, sometimes a
// moment after the call returns, so the script waits for it. It must never
// create the key itself: a CLSID we invented would be a phantom entry.
func TestCloudRootPinScriptWaitsForTheNodeAndNeverCreatesIt(t *testing.T) {
	s := cloudRootPinScript(testCloudRoot, false)
	if !strings.Contains(s, "Test-Path -LiteralPath $k") {
		t.Errorf("script does not check the node exists:\n%s", s)
	}
	if !strings.Contains(s, "Start-Sleep") {
		t.Errorf("script does not wait for a node that is still being created:\n%s", s)
	}
	if strings.Contains(s, "New-Item -") {
		t.Errorf("script creates keys; it must only set a value on the node Windows made:\n%s", s)
	}
}

// The value name is Windows' own spelling on the nodes it generates.
func TestCloudRootPinnedValueName(t *testing.T) {
	if cloudRootPinnedValue != "System.IsPinnedToNamespaceTree" {
		t.Errorf("cloudRootPinnedValue = %q", cloudRootPinnedValue)
	}
}
