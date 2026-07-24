//go:build windows

package policy

import (
	"testing"

	"golang.org/x/sys/windows/registry"
)

func TestLoadUnmanaged(t *testing.T) {
	// A path that doesn't exist → unmanaged, sign-out allowed.
	p := loadFrom(registry.CURRENT_USER, `Software\NimboPolicyTest_DoesNotExist`)
	if p.Managed {
		t.Error("absent key reported as managed")
	}
	if !p.AllowSignOut {
		t.Error("unmanaged default must allow sign-out")
	}
}

func TestLoadManaged(t *testing.T) {
	const path = `Software\NimboPolicyTest`
	k, _, err := registry.CreateKey(registry.CURRENT_USER, path, registry.WRITE)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { registry.DeleteKey(registry.CURRENT_USER, path) })
	_ = k.SetStringValue("ServerURL", "https://cloud.example.com")
	_ = k.SetDWordValue("LockServer", 1)
	_ = k.SetDWordValue("AllowSignOut", 0)
	_ = k.SetDWordValue("LockBandwidth", 1)
	_ = k.SetDWordValue("UploadKBps", 2000)
	_ = k.SetDWordValue("DownloadKBps", 500)
	_ = k.SetStringValue("SyncMode", "ondemand")
	k.Close()

	p := loadFrom(registry.CURRENT_USER, path)
	if !p.Managed {
		t.Fatal("present key not reported as managed")
	}
	if p.ServerURL != "https://cloud.example.com" || !p.LockServer {
		t.Errorf("server policy = %q lock=%v", p.ServerURL, p.LockServer)
	}
	if p.AllowSignOut {
		t.Error("AllowSignOut=0 should disable sign-out")
	}
	if !p.LockBandwidth || p.UploadKBps != 2000 || p.DownloadKBps != 500 {
		t.Errorf("bandwidth policy = %+v", p)
	}
	if p.SyncMode != "ondemand" {
		t.Errorf("syncMode = %q", p.SyncMode)
	}
}

func TestLoadIgnoresInvalidSyncMode(t *testing.T) {
	const path = `Software\NimboPolicyTest2`
	k, _, err := registry.CreateKey(registry.CURRENT_USER, path, registry.WRITE)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { registry.DeleteKey(registry.CURRENT_USER, path) })
	_ = k.SetStringValue("SyncMode", "garbage")
	// AllowSignOut absent → must default to true even on a managed key.
	k.Close()

	p := loadFrom(registry.CURRENT_USER, path)
	if p.SyncMode != "" {
		t.Errorf("invalid SyncMode should be ignored, got %q", p.SyncMode)
	}
	if !p.AllowSignOut {
		t.Error("absent AllowSignOut should default to true")
	}
}
