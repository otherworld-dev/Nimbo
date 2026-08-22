//go:build windows

package transfer

import (
	"fmt"
	"syscall"
	"unsafe"
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
