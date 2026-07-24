//go:build linux

package autostart

import (
	"os"
	"strings"
	"testing"
)

func TestLinuxAutostart(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if !Supported() {
		t.Fatal("autostart should be supported on linux")
	}
	if ok, err := Enabled(); err != nil || ok {
		t.Fatalf("should start disabled, got %v / %v", ok, err)
	}

	if err := Enable("/opt/nimbo/nimbo-gui"); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if ok, err := Enabled(); err != nil || !ok {
		t.Fatalf("Enabled after Enable = %v / %v", ok, err)
	}
	b, err := os.ReadFile(desktopPath())
	if err != nil {
		t.Fatalf("read desktop file: %v", err)
	}
	if !strings.Contains(string(b), "Exec=/opt/nimbo/nimbo-gui\n") {
		t.Errorf("desktop file missing/unquoted Exec:\n%s", b)
	}

	if err := Disable(); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if ok, _ := Enabled(); ok {
		t.Fatal("should be disabled after Disable")
	}
	if err := Disable(); err != nil { // idempotent when absent
		t.Errorf("Disable on absent entry should be a no-op, got %v", err)
	}
}

func TestLinuxAutostartQuotesSpaces(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := Enable("/home/a b/nimbo gui"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(desktopPath())
	if !strings.Contains(string(b), `Exec="/home/a b/nimbo gui"`+"\n") {
		t.Errorf("a path with spaces must be quoted:\n%s", b)
	}
}
