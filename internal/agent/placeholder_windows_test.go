package agent

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestIsDehydratedPlaceholderRealFile drives the detector from a real on-disk
// file. A true cloud placeholder can only be created by a sync provider's
// filter driver, but FILE_ATTRIBUTE_OFFLINE carries the same meaning ("the
// contents are not on this disk") and CAN be set directly, so it stands in for
// one end to end: os.Stat -> attribute bits -> decision.
func TestIsDehydratedPlaceholderRealFile(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain.txt")
	if err := os.WriteFile(plain, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(plain)
	if err != nil {
		t.Fatal(err)
	}
	if isDehydratedPlaceholder(fi) {
		t.Error("a normal file must not read as dehydrated")
	}

	stub := filepath.Join(dir, "stub.txt")
	if err := os.WriteFile(stub, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := syscall.UTF16PtrFromString(stub)
	if err != nil {
		t.Fatal(err)
	}
	attrs, err := syscall.GetFileAttributes(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.SetFileAttributes(p, attrs|fileAttributeOffline); err != nil {
		t.Skipf("cannot set FILE_ATTRIBUTE_OFFLINE on this system: %v", err)
	}
	if got, err := syscall.GetFileAttributes(p); err != nil {
		t.Fatal(err)
	} else if got&fileAttributeOffline == 0 {
		t.Skip("FILE_ATTRIBUTE_OFFLINE was silently dropped by the filesystem")
	}

	fi, err = os.Stat(stub)
	if err != nil {
		t.Fatal(err)
	}
	if !isDehydratedPlaceholder(fi) {
		t.Error("a file marked offline must read as dehydrated")
	}
}

func TestIsDehydratedPlaceholderNil(t *testing.T) {
	if isDehydratedPlaceholder(nil) {
		t.Error("nil FileInfo must not read as dehydrated")
	}
}
