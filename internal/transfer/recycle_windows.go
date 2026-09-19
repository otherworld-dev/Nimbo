//go:build windows

package transfer

import (
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// recycle sends a path to the Windows Recycle Bin, silently.
//
// SHFileOperationW rather than IFileOperation: no COM apartment to negotiate,
// and the sync loop calls this from worker goroutines. FOF_ALLOWUNDO is what
// makes it a recycle rather than a delete; the bin itself enforces its size
// cap, aging out the oldest entries, so this cannot grow without bound.
func recycle(path string) error {
	// The API takes a DOUBLE-null-terminated list of null-terminated paths.
	from, err := syscall.UTF16FromString(path)
	if err != nil {
		return err
	}
	from = append(from, 0)

	const (
		foDelete          = 3      // FO_DELETE
		fofAllowUndo      = 0x0040 // FOF_ALLOWUNDO — the Recycle Bin, not a delete
		fofNoConfirmation = 0x0010 // FOF_NOCONFIRMATION
		fofSilent         = 0x0004 // FOF_SILENT — no progress dialog
		fofNoErrorUI      = 0x0400 // FOF_NOERRORUI
	)
	// SHFILEOPSTRUCTW, laid out for 64-bit Windows.
	type shFileOp struct {
		hwnd                  uintptr
		wFunc                 uint32
		pFrom                 *uint16
		pTo                   *uint16
		fFlags                uint16
		fAnyOperationsAborted int32
		hNameMappings         uintptr
		lpszProgressTitle     *uint16
	}
	op := shFileOp{
		wFunc:  foDelete,
		pFrom:  &from[0],
		fFlags: fofAllowUndo | fofNoConfirmation | fofSilent | fofNoErrorUI,
	}
	ret, _, _ := procSHFileOperationW.Call(uintptr(unsafe.Pointer(&op)))
	if ret != 0 {
		return fmt.Errorf("recycle %s: SHFileOperation error %#x", path, ret)
	}
	if op.fAnyOperationsAborted != 0 {
		return fmt.Errorf("recycle %s: aborted", path)
	}
	return nil
}

var (
	shell32              = syscall.NewLazyDLL("shell32.dll")
	procSHFileOperationW = shell32.NewProc("SHFileOperationW")
)

// volumeBinCapacity reports the largest item the Recycle Bin on path's volume
// will keep, in bytes: 0 when that volume has no bin (a network or removable
// drive) or the bin is switched off, since Windows deletes outright there.
// Explorer keeps each volume's setting under BitBucket\Volume\{GUID}; a volume
// never configured has no key, and then a conservative 5% of the drive is
// assumed, below the Windows default, so the guess errs towards moving aside.
func volumeBinCapacity(path string) (int64, bool) {
	root := filepath.VolumeName(filepath.Clean(path)) + `\`
	rootp, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return 0, true
	}
	if windows.GetDriveType(rootp) != windows.DRIVE_FIXED {
		return 0, true
	}
	var total uint64
	if err := windows.GetDiskFreeSpaceEx(rootp, nil, &total, nil); err != nil {
		return 0, true
	}
	guess := int64(total / 20)
	buf := make([]uint16, 64)
	if err := windows.GetVolumeNameForVolumeMountPoint(rootp, &buf[0], uint32(len(buf))); err != nil {
		return guess, true
	}
	vol := windows.UTF16ToString(buf) // \\?\Volume{GUID}\
	i, j := strings.Index(vol, "{"), strings.Index(vol, "}")
	if i < 0 || j < i {
		return guess, true
	}
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Explorer\BitBucket\Volume\`+vol[i:j+1], registry.QUERY_VALUE)
	if err != nil {
		return guess, true
	}
	defer k.Close()
	if nuke, _, err := k.GetIntegerValue("NukeOnDelete"); err == nil && nuke != 0 {
		return 0, true
	}
	if mb, _, err := k.GetIntegerValue("MaxCapacity"); err == nil && mb > 0 {
		return int64(mb) << 20, true
	}
	return guess, true
}
