package cfapi

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
	if IsDehydrated(fi) {
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
	if !IsDehydrated(fi) {
		t.Error("a file marked offline must read as dehydrated")
	}
}

func TestIsDehydratedPlaceholderNil(t *testing.T) {
	if IsDehydrated(nil) {
		t.Error("nil FileInfo must not read as dehydrated")
	}
}

// Windows' safe-save (ReplaceFile) copies the replaced file's attributes onto
// the new copy, so saving over an online-only placeholder leaves a plain file
// holding all its bytes yet still marked FILE_ATTRIBUTE_OFFLINE, which reads
// as dehydrated (Deck #793). ClearStrayOffline takes that bit off a plain
// file and leaves every other attribute alone.
func TestClearStrayOfflineRealFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "saved.ods")
	if err := os.WriteFile(p, []byte("version 2"), 0o644); err != nil {
		t.Fatal(err)
	}
	u, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		t.Fatal(err)
	}
	attrs, err := syscall.GetFileAttributes(u)
	if err != nil {
		t.Fatal(err)
	}
	if cleared, err := ClearStrayOffline(p); err != nil || cleared {
		t.Fatalf("a file without the offline bit: cleared=%v err=%v, want false, nil", cleared, err)
	}
	if err := syscall.SetFileAttributes(u, attrs|fileAttributeOffline|syscall.FILE_ATTRIBUTE_READONLY); err != nil {
		t.Skipf("cannot set FILE_ATTRIBUTE_OFFLINE on this system: %v", err)
	}
	t.Cleanup(func() { _ = syscall.SetFileAttributes(u, syscall.FILE_ATTRIBUTE_NORMAL) })
	if got, _ := syscall.GetFileAttributes(u); got&fileAttributeOffline == 0 {
		t.Skip("FILE_ATTRIBUTE_OFFLINE was silently dropped by the filesystem")
	}

	cleared, err := ClearStrayOffline(p)
	if err != nil || !cleared {
		t.Fatalf("cleared=%v err=%v, want true, nil", cleared, err)
	}
	got, err := syscall.GetFileAttributes(u)
	if err != nil {
		t.Fatal(err)
	}
	if got&fileAttributeOffline != 0 {
		t.Errorf("attributes 0x%x still carry FILE_ATTRIBUTE_OFFLINE", got)
	}
	if got&syscall.FILE_ATTRIBUTE_READONLY == 0 {
		t.Errorf("attributes 0x%x lost FILE_ATTRIBUTE_READONLY", got)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if IsDehydrated(fi) {
		t.Error("the saved file still reads as dehydrated")
	}
}

// The recall bits are set only by the cloud filter or an HSM driver and mean
// the bytes really are elsewhere; a file carrying one keeps its OFFLINE bit.
func TestStrayOfflineOnlyWithoutRecallBits(t *testing.T) {
	cases := []struct {
		attrs uint32
		want  bool
	}{
		{0x20, false},
		{0x20 | fileAttributeOffline, true},
		{0x20 | fileAttributeOffline | fileAttributeRecallOnDataAccess, false},
		{0x20 | fileAttributeOffline | fileAttributeRecallOnOpen, false},
		{0x20 | fileAttributeOffline | syscall.FILE_ATTRIBUTE_REPARSE_POINT, false}, // a placeholder: not ours to touch
		{syscall.FILE_ATTRIBUTE_DIRECTORY | fileAttributeOffline, false},
	}
	for _, c := range cases {
		if got := strayOffline(c.attrs); got != c.want {
			t.Errorf("strayOffline(0x%x) = %v, want %v", c.attrs, got, c.want)
		}
	}
}
